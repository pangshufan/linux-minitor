package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"monitor/protocol"
)

func testTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	pair := func(serial int64, client bool) tls.Certificate {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, DNSNames: []string{"monitor.test"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if client {
			u, _ := url.Parse("urn:monitor:host:node")
			leaf.URIs = []*url.URL{u}
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		b, err := x509.CreateCertificate(rand.Reader, leaf, cert, &k.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{b}, PrivateKey: k}
	}
	return &tls.Config{Certificates: []tls.Certificate{pair(2, false)}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12},
		&tls.Config{Certificates: []tls.Certificate{pair(3, true)}, RootCAs: pool, ServerName: "monitor.test", MinVersion: tls.VersionTLS12}
}

func TestOptionalTLSAndToken(t *testing.T) {
	serverTLS, clientTLS := testTLS(t)
	for _, tt := range []struct {
		name                       string
		tls                        bool
		wantToken, sendToken, host string
		code                       codes.Code
	}{
		{name: "plaintext", host: "node"},
		{name: "plaintext-token", wantToken: "secret", sendToken: "secret", host: "node"},
		{name: "missing-token", wantToken: "secret", host: "node", code: codes.Unauthenticated},
		{name: "wrong-token", wantToken: "secret", sendToken: "bad", host: "node", code: codes.Unauthenticated},
		{name: "invalid-plaintext-id", host: "../node", code: codes.PermissionDenied},
		{name: "mtls", tls: true, host: "node"},
		{name: "mtls-host-mismatch", tls: true, host: "other", code: codes.PermissionDenied},
		{name: "mtls-token", tls: true, wantToken: "secret", sendToken: "secret", host: "node"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, err := openStore(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.db.Close()
			h := &hub{store: db, plaintext: !tt.tls, authToken: tt.wantToken, offlineAfter: time.Second, sessions: map[string]*session{}}
			listener := bufconn.Listen(1024 * 1024)
			defer listener.Close()
			var options []grpc.ServerOption
			var creds credentials.TransportCredentials = insecure.NewCredentials()
			if tt.tls {
				options = append(options, grpc.Creds(credentials.NewTLS(serverTLS)))
				creds = credentials.NewTLS(clientTLS)
			}
			gs := grpc.NewServer(options...)
			protocol.RegisterMonitorServer(gs, h)
			go gs.Serve(listener)
			defer gs.Stop()
			conn, err := grpc.NewClient("passthrough:///buffer", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(creds))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tt.sendToken != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tt.sendToken)
			}
			stream, err := protocol.NewMonitorClient(conn).Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_Hello{Hello: &protocol.Hello{HostId: tt.host, RunId: "00000000000000000000000000000001", ReportIntervalMs: 1000}}})
			reply, err := stream.Recv()
			if status.Code(err) != tt.code {
				t.Fatalf("code=%s want=%s: %v", status.Code(err), tt.code, err)
			}
			if tt.code != codes.OK {
				return
			}
			if reply.GetAck() == nil {
				t.Fatal("registration ack missing")
			}
			// Verify the existing stream still carries snapshots, heartbeats and reverse queries.
			err = stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_Heartbeat{Heartbeat: &protocol.Heartbeat{LatestSampleId: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			if reply, err = stream.Recv(); err != nil || reply.GetAck() == nil {
				t.Fatalf("heartbeat: %v", err)
			}
			snap := &protocol.Snapshot{MetricsJson: []byte(`{"sample_id":1,"collected_at":"2026-11-08T12:00:00Z","processes":{"enabled":true,"items":[{"pid":42}]}}`)}
			if err := stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_Snapshot{Snapshot: snap}}); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { _, err := h.query(ctx, tt.host, protocol.Query_KIND_LATEST); result <- err }()
			reply, err = stream.Recv()
			if err != nil || reply.GetQuery() == nil {
				t.Fatalf("reverse query: %v", err)
			}
			if err := stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_QueryResult{QueryResult: &protocol.QueryResult{RequestId: reply.GetQuery().RequestId, Snapshot: snap}}}); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("GET", "/api/hosts/"+tt.host, nil)
			req.SetBasicAuth("monitor", "test")
			rec := httptest.NewRecorder()
			h.webRoutes("monitor", "test").ServeHTTP(rec, req)
			var record hostRecord
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &record) != nil || !record.Online || len(record.Metrics) == 0 {
				t.Fatalf("Web API roundtrip: %s", rec.Body.String())
			}

		})
	}
}
