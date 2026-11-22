package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"monitor/internal/transport"

	"crypto/tls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestBootstrapTemplatesAndIssuedAgentHandshake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PKI shell wizard is tested on Unix; generated files are portable")
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("OpenSSL is required to test PKI bootstrap")
	}
	if version, err := exec.Command("openssl", "version").Output(); err != nil || !bytes.HasPrefix(version, []byte("OpenSSL ")) {
		t.Skip("PKI bootstrap requires OpenSSL 1.1.1+ (not LibreSSL)")
	}
	output := filepath.Join(t.TempDir(), "config")
	bootstrap := func() ([]byte, error) {
		cmd := exec.Command("bash", "../deploy/init-server-pki.sh", output)
		cmd.Stdin = strings.NewReader("192.0.2.10\nmonitor.example.com\n")
		return cmd.CombinedOutput()
	}
	if data, err := bootstrap(); err != nil {
		t.Fatalf("PKI wizard: %v\n%s", err, data)
	}
	var serverTLS *tls.Config
	var issuer *agentIssuer
	// Both shipped templates must reference exactly the PKI files the wizard generates.
	for _, name := range []string{"server-sqlite.json", "server-postgres.json"} {
		data, err := os.ReadFile(filepath.Join("../configs", name))
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		path := func(key string) string {
			value, _ := cfg[key].(string)
			if !strings.HasPrefix(value, "config/") {
				t.Fatalf("%s has unexpected %s: %s", name, key, value)
			}
			return filepath.Join(output, strings.TrimPrefix(value, "config/"))
		}
		if cfg["tls-enabled"] != true || cfg["web-tls-enabled"] != true {
			t.Fatal("Recommended template must enable both TLS links")
		}
		serverTLS, err = transport.TLSConfig(path("tls-cert"), path("tls-key"), path("tls-ca"))
		if err != nil {
			t.Fatal(err)
		}
		serverTLS.ClientAuth = tls.RequireAndVerifyClientCert
		if _, err := tls.LoadX509KeyPair(path("web-cert"), path("web-key")); err != nil {
			t.Fatal(err)
		}
		issuer, err = loadAgentIssuer(path("agent-issuer-cert"), path("agent-issuer-key"), path("agent-server-ca"), serverTLS)
		if err != nil {
			t.Fatalf("%s is not ready for Web issuance: %v", name, err)
		}
	}
	before, _ := os.ReadFile(filepath.Join(output, "agent-ca.crt"))
	if _, err := bootstrap(); err == nil {
		t.Fatal("Wizard must not overwrite an existing PKI")
	}
	after, _ := os.ReadFile(filepath.Join(output, "agent-ca.crt"))
	if !bytes.Equal(before, after) {
		t.Fatal("Existing CA was replaced")
	}
	now := time.Now().UTC().Truncate(time.Second)
	bundle, _, err := issuer.agentBundle(agentCertificateRequest{HostID: "bootstrap-node", Platform: "windows", Server: "192.0.2.10:8443"}, now, now.Add(400*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f.Name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	clientTLS, err := transport.TLSConfig(filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	clientTLS.ServerName = "192.0.2.10"
	listener := bufconn.Listen(1024 * 1024)
	defer listener.Close()
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///bootstrap", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/bootstrap.Probe/TLS", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("Generated template and Web-issued Agent must complete mTLS without changing CA files: %v", err)
	}
}

func TestCertificatePageExplainsDisabledState(t *testing.T) {
	for _, tc := range []struct {
		issuer      *agentIssuer
		url, reason string
	}{
		{nil, "https://monitor.test/api/agent-certificates", "Server 未配置签发 CA"},
		{&agentIssuer{}, "http://monitor.test/api/agent-certificates", "HTTPS"},
	} {
		h := &hub{agentIssuer: tc.issuer}
		mux := http.NewServeMux()
		h.agentCertificateRoutes(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", tc.url, nil))
		var response struct {
			Enabled bool   `json:"enabled"`
			Reason  string `json:"disabled_reason"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &response) != nil || response.Enabled || !strings.Contains(response.Reason, tc.reason) {
			t.Fatalf("Missing actionable explanation: %s", rec.Body.String())
		}
	}
}
