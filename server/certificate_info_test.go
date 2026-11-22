package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"monitor/protocol"
)

func TestHostCertificateSurvivesDisconnectAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "certificates.db")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.db.Close() }()
	if err := db.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 1000}, time.Now()); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	info := &certificateInfo{Usage: "Agent", NotAfter: expires}
	if err := db.saveHostCertificate(ctx, "node", info); err != nil {
		t.Fatal(err)
	}
	db.db.Close()
	db, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.hosts(ctx, "node")
	if err != nil || len(rows) != 1 {
		t.Fatalf("hosts: %v %v", rows, err)
	}
	var actual certificateInfo
	if err := json.Unmarshal(rows[0].Certificate, &actual); err != nil || !actual.NotAfter.Equal(expires) {
		t.Fatalf("certificate after restart: %v %v", actual, err)
	}
	// A plaintext re-registration must clear the stale TLS information.
	if err := db.saveHostCertificate(ctx, "node", nil); err != nil {
		t.Fatal(err)
	}
	rows, err = db.hosts(ctx, "node")
	if err != nil || len(rows[0].Certificate) != 0 {
		t.Fatalf("stale certificate on plaintext host: %v %v", rows, err)
	}
	// The foreign key handles retirement without leaving certificate rows behind.
	if err := db.saveHostCertificate(ctx, "node", info); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(ctx, "DELETE FROM hosts WHERE host_id=?", "node"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM host_certificates").Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan certificate count: %d %v", count, err)
	}
}

func TestServerCertificateVisibility(t *testing.T) {
	server, _ := testTLS(t)
	cert, err := x509.ParseCertificate(server.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(server.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	for path, block := range map[string]*pem.Block{
		certPath: {Type: "CERTIFICATE", Bytes: cert.Raw},
		keyPath:  {Type: "PRIVATE KEY", Bytes: key},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := loadedServerCertificates(false, "", "", false, "", "", nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("plaintext displayed certificates: %v %v", rows, err)
	}
	rows, err = loadedServerCertificates(true, certPath, keyPath, true, certPath, keyPath,
		&agentIssuer{certificate: cert})
	if err != nil || len(rows) != 3 {
		t.Fatalf("TLS displayed certificates: %v %v", rows, err)
	}
	for _, info := range rows {
		if !info.NotAfter.Equal(cert.NotAfter) || len(info.Fingerprint) != 64 {
			t.Fatalf("wrong certificate description: %+v", info)
		}
	}
}
