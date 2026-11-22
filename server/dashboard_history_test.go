package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"monitor/protocol"
)

func TestDashboardHistorySelectsOneProcessAndIOWait(t *testing.T) {
	ctx := context.Background()
	db, err := openStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 1000}, now); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{
		"sample_id": 1, "collected_at": now.Format(time.RFC3339),
		"processes": map[string]any{"services": []any{map[string]any{"key": "service/app", "rss_sum_bytes": 100}},
			"items": []any{map[string]any{"key": "process/11/1", "pid": 11, "rss_bytes": 100}, map[string]any{"key": "process/12/1", "pid": 12}}},
		"windows": []any{map[string]any{"requested_ms": 1000, "elapsed_ms": 1000, "cpu_percent": map[string]any{"cpu": map[string]any{"busy": 25, "iowait": 4}},
			"rates": map[string]any{"disk/8:0/sda": map[string]any{"util_percent": 80, "avg_queue_depth": 2},
				"service/app": map[string]any{"cpu_percent": 50}, "process/11/1": map[string]any{"cpu_percent": 30}, "process/12/1": map[string]any{"cpu_percent": 20}}}},
	}
	body, _ := json.Marshal(m)
	if err := db.save(ctx, "node", "run", &protocol.Snapshot{MetricsJson: body}, now); err != nil {
		t.Fatal(err)
	}
	selected, err := db.history(ctx, "node", now.Add(-time.Hour), 1000, "process/11/1")
	if err != nil || len(selected) != 1 {
		t.Fatalf("history: %v %v", selected, err)
	}
	point := selected[0]
	var processes []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(point.Processes, &processes); err != nil || len(processes) != 1 || processes[0].Key != "process/11/1" {
		t.Fatalf("wrong selected process: %s %v", point.Processes, err)
	}
	if point.IOWait == nil || *point.IOWait != 4 || point.Rates["disk/8:0/sda"]["util_percent"] != 80 {
		t.Fatalf("missing bottleneck metrics: %+v", point)
	}
	if _, ok := point.Rates["process/12/1"]; ok {
		t.Fatal("unselected process rates must be omitted")
	}
	base, err := db.history(ctx, "node", now.Add(-time.Hour), 1000)
	if err != nil || len(base) != 1 || len(base[0].Processes) != 0 {
		t.Fatalf("default history must omit process detail: %+v %v", base, err)
	}
	if _, ok := base[0].Rates["process/11/1"]; ok {
		t.Fatal("default history should retain service aggregates instead of all process rate maps")
	}
}
