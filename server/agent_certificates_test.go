package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monitor/internal/transport"
	"monitor/protocol"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

func TestAgentCertificateEnrollment(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	makeCert := func(template, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if parent == nil {
			parent, parentKey = template, key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}
	root, rootKey, rootPEM, rootKeyPEM := makeCert(&x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Original CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, MaxPathLenZero: true,
	}, nil, nil)
	// A dedicated CA allows enrollment without putting the original CA key online.
	_, _, issuerPEM, issuerKey := makeCert(&x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Agent CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(0, 0, 730),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, MaxPathLenZero: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, nil, nil)
	_, _, serverPEM, serverKey := makeCert(&x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "monitor.test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(0, 0, 180),
		IPAddresses: []net.IP{net.ParseIP("10.181.21.20")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage: x509.KeyUsageDigitalSignature,
	}, root, rootKey)
	serverTLS, err := transport.TLSConfig(write("server.crt", serverPEM), write("server.key", serverKey),
		write("client-ca.pem", bytes.Join([][]byte{rootPEM, issuerPEM}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	serverTLS.ClientAuth = tls.RequireAndVerifyClientCert
	issuerPath, issuerKeyPath := write("issuer.crt", issuerPEM), write("issuer.key", issuerKey)
	// Even an incorrectly combined CA input must never disclose a private key in the download.
	serverCAPath := write("server-ca.pem", bytes.Join([][]byte{rootPEM, rootKeyPEM}, nil))
	untrustedTLS := serverTLS.Clone()
	untrustedTLS.ClientCAs = x509.NewCertPool()
	untrustedTLS.ClientCAs.AddCert(root)
	if _, err := loadAgentIssuer(issuerPath, issuerKeyPath, serverCAPath, untrustedTLS); err == nil || !strings.Contains(err.Error(), "not trusted by tls-ca") {
		t.Fatalf("untrusted Agent CA must fail with actionable trust configuration error: %v", err)
	}
	if _, err := loadAgentIssuer(issuerPath, issuerKeyPath, write("wrong-server-ca.pem", issuerPEM), serverTLS); err == nil {
		t.Fatal("Agent download CA that cannot verify the gRPC Server was accepted")
	}
	issuer, err := loadAgentIssuer(issuerPath, issuerKeyPath, serverCAPath, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	h := &hub{store: db, agentIssuer: issuer, offlineAfter: 15 * time.Second, sessions: make(map[string]*session)}
	handler := h.webRoutes("monitor", "test")
	const body = `{"host_id":"win-node-01","server":"10.181.21.20:8443","platform":"windows","valid_days":90}`
	for _, tt := range []struct {
		name, scheme, origin, body string
		auth                       bool
		status                     int
	}{
		{"missing auth", "https", "", body, false, http.StatusUnauthorized},
		{"insecure", "http", "", body, true, http.StatusForbidden},
		{"external browser", "https", "https://other.test", body, true, http.StatusForbidden},
		{"wrong SAN", "https", "", strings.Replace(body, "10.181.21.20", "10.181.21.21", 1), true, http.StatusBadRequest},
		{"invalid identity", "https", "", strings.Replace(body, "win-node-01", "../node", 1), true, http.StatusBadRequest},
		{"long validity allowed", "https", "", strings.Replace(body, ":90", ":400", 1), true, http.StatusOK},
		{"zero days", "https", "", strings.Replace(body, ":90", ":0", 1), true, http.StatusBadRequest},
		{"huge days", "https", "", strings.Replace(body, ":90", ":9223372036854775807", 1), true, http.StatusBadRequest},
		{"expired chain", "https", "", strings.Replace(body, ":90", ":731", 1), true, http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tt.scheme+"://monitor.test/api/agent-certificates", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.auth {
				req.SetBasicAuth("monitor", "test")
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if tt.name == "long validity allowed" {
				archive, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
				if err != nil {
					t.Fatal(err)
				}
				file, err := archive.Open("agent.crt")
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(file)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
				block, _ := pem.Decode(data)
				if block == nil {
					t.Fatal("missing certificate PEM")
				}
				leaf, err := x509.ParseCertificate(block.Bytes)
				if err != nil || time.Until(leaf.NotAfter) < 399*24*time.Hour {
					t.Fatalf("long validity was truncated: %v %v", leaf, err)
				}
			}
		})
	}
	req := httptest.NewRequest("POST", "https://monitor.test/api/agent-certificates", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://monitor.test")
	req.SetBasicAuth("monitor", "test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" ||
		rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("download status %d headers %v", rec.Code, rec.Header())
	}
	archive, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte)
	for _, f := range archive.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name], err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if f.Mode().Perm() != 0600 {
			t.Fatalf("unsafe mode for %s: %v", f.Name, f.Mode())
		}
	}
	var config map[string]any
	if err := json.Unmarshal(files["agent.json"], &config); err != nil {
		t.Fatal(err)
	}
	if config["host-id"] != "win-node-01" || config["server"] != "10.181.21.20:8443" ||
		config["tls-key"] != "C:/ProgramData/Monitor/agent.key" || config["tls-enabled"] != true {
		t.Fatalf("unexpected Agent config: %v", config)
	}
	if !bytes.Equal(files["ca.crt"], rootPEM) {
		t.Fatal("download should contain only the public gRPC Server CA")
	}
	clientTLS, err := transport.TLSConfig(write("agent.crt", files["agent.crt"]), write("agent.key", files["agent.key"]), write("ca.crt", files["ca.crt"]))
	if err != nil {
		t.Fatal(err)
	}
	clientTLS.ServerName = "10.181.21.20"
	listener := bufconn.Listen(1024 * 1024)
	defer listener.Close()
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	protocol.RegisterMonitorServer(gs, h)
	go gs.Serve(listener)
	defer gs.Stop()
	conn, err := grpc.NewClient("passthrough:///buffer",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := protocol.NewMonitorClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_Hello{Hello: &protocol.Hello{
		HostId: "win-node-01", RunId: "00000000000000000000000000000001", ReportIntervalMs: 1000,
	}}}); err != nil {
		t.Fatal(err)
	}
	if reply, err := stream.Recv(); err != nil || reply.GetAck() == nil {
		t.Fatalf("enrolled Agent did not register: %v, %v", reply, err)
	}
}
