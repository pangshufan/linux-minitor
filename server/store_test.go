package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"monitor/protocol"
)

func TestSnapshotVersionsAndProcessRestarts(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	testStoreRoundTrip(t, db)
}

// A fresh schema isolates this test from any existing application tables.
func TestPostgresRoundTrip(t *testing.T) {
	dsn := os.Getenv("MONITOR_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set MONITOR_TEST_POSTGRES_DSN to run against PostgreSQL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("monitor_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA " + name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec("DROP SCHEMA " + name + " CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = name
	isolated := stdlib.RegisterConnConfig(cfg)
	defer stdlib.UnregisterConnConfig(isolated)
	db, err := openDatabase("postgres", isolated)
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	testStoreRoundTrip(t, db)
}

func testStoreRoundTrip(t *testing.T, db *store) {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if err := db.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 30000}, start); err != nil {
		t.Fatal(err)
	}
	save := func(run string, id int, at time.Time, config string) {
		t.Helper()
		msg := &protocol.Snapshot{
			MetricsJson:   []byte(fmt.Sprintf(`{"sample_id":%d,"collected_at":%q,"memory_bytes":{"used":1,"total":2},"processes":{"enabled":true,"services":[{"key":"service/app","name":"app","rss_sum_bytes":4096}],"items":[{"pid":42}]}}`, id, at.Format(time.RFC3339))),
			InventoryJson: []byte(fmt.Sprintf(`{"host_id":"node","collected_at":%q,"system":{"hostname":%q}}`, at.Format(time.RFC3339), config)),
		}
		if run == "run-1" {
			msg.FilesystemsJson = []byte(`{"mounts":[]}`)
		}
		if err := db.save(ctx, "node", run, msg, at); err != nil {
			t.Fatal(err)
		}
	}
	save("run-1", 1, start, "before")
	save("run-1", 1, start.Add(time.Second), "before")
	save("run-2", 1, start.Add(2*time.Second), "after")
	points, err := db.history(ctx, "node", start)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].InventoryVersion == points[1].InventoryVersion || *points[1].Memory != 50 {
		t.Fatalf("unexpected history: %+v", points)
	}
	hostsBeforePrune, err := db.hosts(ctx, "node")
	if err != nil || len(hostsBeforePrune) != 1 {
		t.Fatalf("hosts: %v %v", hostsBeforePrune, err)
	}
	var metrics struct {
		Processes struct {
			Items []struct {
				PID int `json:"pid"`
			} `json:"items"`
		} `json:"processes"`
	}
	if err := json.Unmarshal(hostsBeforePrune[0].Metrics, &metrics); err != nil || len(metrics.Processes.Items) != 1 || metrics.Processes.Items[0].PID != 42 {
		t.Fatalf("process data not persisted: %s", hostsBeforePrune[0].Metrics)
	}
	if len(points[0].Services) == 0 {
		t.Fatal("service summaries missing from history")
	}
	var versions int
	if err := db.db.QueryRow("SELECT count(*) FROM inventories").Scan(&versions); err != nil || versions != 2 {
		t.Fatalf("inventory versions=%d, err=%v", versions, err)
	}
	if err := db.prune(ctx, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	hosts, err := db.hosts(ctx, "node")
	if err != nil || len(hosts) != 1 || len(hosts[0].Metrics) == 0 || len(hosts[0].Filesystems) == 0 {
		t.Fatalf("latest sample should survive history expiry: %v, %v", hosts, err)
	}
}
