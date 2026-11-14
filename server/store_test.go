package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	cfgAlerts, err := loadAlertConfig(`{"enabled":true,"rules":{"service_stopped":{"trigger_count":1}},"receivers":{"test":{"url":"https://receiver.example/alerts"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db.alerts, err = newAlertEngine(ctx, db, cfgAlerts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	snapshot := &protocol.Snapshot{MetricsJson: []byte(fmt.Sprintf(
		`{"sample_id":2,"collected_at":%q,"processes":{"services":[{"key":"service/nginx","name":"nginx","state":"stopped"}]}}`, now.Format(time.RFC3339Nano)))}
	if err := db.save(ctx, "node", "run-2", snapshot, now); err != nil {
		t.Fatal(err)
	}
	if processed, err := db.alerts.deliverOne(ctx, &http.Client{Transport: alertRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}, now); err != nil || !processed {
		t.Fatalf("PostgreSQL delivery: processed=%t err=%v", processed, err)
	}
	var delivered int
	if err := db.db.QueryRow("SELECT count(*) FROM alert_deliveries WHERE status='delivered'").Scan(&delivered); err != nil || delivered != 1 {
		t.Fatalf("PostgreSQL delivered=%d err=%v", delivered, err)
	}
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
	points, err := db.history(ctx, "node", start, 0)
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

func TestHistoryCursorIncludesAllSamplesAtBoundary(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	ctx := context.Background()
	boundary := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	if err := store.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 1000}, boundary); err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Time{boundary.Add(-time.Second), boundary, boundary} {
		payload, err := json.Marshal(metricHeader{SampleID: int64(i + 1), CollectedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.save(ctx, "node", "run", &protocol.Snapshot{MetricsJson: payload}, at); err != nil {
			t.Fatal(err)
		}
	}

	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/hosts/node/history?range=1h&since_ms=%d", boundary.UnixMilli()), nil)
	request.SetPathValue("id", "node")
	response := httptest.NewRecorder()
	(&hub{store: store}).hostHistory(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("history status %d: %s", response.Code, response.Body.String())
	}
	var points []historyPoint
	if err := json.Unmarshal(response.Body.Bytes(), &points); err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].ReceivedMS != boundary.UnixMilli() || points[1].ReceivedMS != boundary.UnixMilli() {
		t.Fatalf("expected both boundary samples, got %+v", points)
	}
}

func TestSQLitePragmasOnReplacementConnection(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	db.db.SetMaxIdleConns(0)
	for _, cfg := range []struct {
		pragma string
		want   int
	}{{"foreign_keys", 1}, {"busy_timeout", 5000}} {
		var got int
		if err := db.db.QueryRow("PRAGMA " + cfg.pragma).Scan(&got); err != nil || got != cfg.want {
			t.Fatalf("%s: got=%d want=%d err=%v", cfg.pragma, got, cfg.want, err)
		}
	}
}

func TestRetentionAndRetirement(t *testing.T) {
	ctx := context.Background()
	db, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	cfg, err := loadAlertConfig(`{"rules":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	db.alerts, err = newAlertEngine(ctx, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.hello(ctx, &protocol.Hello{HostId: "old", ReportIntervalMs: 30000}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"UPDATE hosts SET inventory_version='current' WHERE host_id='old'",
		"INSERT INTO inventories VALUES('old','orphan','{}'),('old','current','{}'),('old','sampled','{}')",
		"INSERT INTO samples VALUES('old','run',1,'',100,'sampled','{}',NULL)",
		"INSERT INTO alert_instances(id,active_key,rule_id,rule_version,host_id,target,state,hits,clears,last_observation,created_ms,updated_ms) VALUES ('old-event',NULL,'cpu','','old','','resolved',0,0,0,0,0),('queued',NULL,'cpu','','old','','resolved',0,0,0,0,0)",
		"INSERT INTO alert_events VALUES('sent','old-event',1,0,'{}'),('waiting','queued',1,0,'{}')",
		"INSERT INTO alert_deliveries(event_id,receiver,status,due_ms) VALUES('sent','demo','delivered',0),('waiting','demo','pending',0)",
	} {
		if _, err := db.exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	assertCount := func(table string, want int) {
		t.Helper()
		var n int
		if err := db.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != want {
			t.Fatalf("%s: count=%d want=%d err=%v", table, n, want, err)
		}
	}
	if err := db.prune(ctx, time.UnixMilli(50)); err != nil {
		t.Fatal(err)
	}
	assertCount("inventories", 2)
	assertCount("samples", 1)
	assertCount("alert_instances", 1)
	assertCount("alert_events", 1)
	assertCount("alert_deliveries", 1)
	if err := db.retire(ctx, "old", time.Now()); err != nil {
		t.Fatal(err)
	}
	assertCount("hosts", 0)
	assertCount("inventories", 0)
	assertCount("samples", 0)
	assertCount("alert_events", 1)
}

func TestHistorySelectsRequestedWindow(t *testing.T) {
	ctx := context.Background()
	db, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	now := time.Now()
	if err := db.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 30000}, now); err != nil {
		t.Fatal(err)
	}
	snap := &protocol.Snapshot{MetricsJson: []byte(fmt.Sprintf(`{"sample_id":1,"collected_at":%q,"windows":[{"requested_ms":1000,"cpu_percent":{"cpu":{"busy":90}}},{"requested_ms":5000,"cpu_percent":{"cpu":{"busy":12}}}]}`, now.Format(time.RFC3339Nano)))}
	if err := db.save(ctx, "node", "run", snap, now); err != nil {
		t.Fatal(err)
	}
	points, err := db.history(ctx, "node", now.Add(-time.Second), 5000)
	if err != nil || len(points) != 1 || points[0].CPU == nil || *points[0].CPU != 12 {
		t.Fatalf("5s history: %+v, %v", points, err)
	}
	points, err = db.history(ctx, "node", now.Add(-time.Second), 30000)
	if err != nil || len(points) != 1 || points[0].CPU != nil || points[0].WindowMS != 30000 {
		t.Fatalf("missing 30s history: %+v, %v", points, err)
	}
}
