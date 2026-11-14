package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monitor/protocol"
)

func TestAlertSnapshotDedupAndWebhookRecoveryAcrossRestart(t *testing.T) {
	events := make(chan alertEvent, 8)
	client := &http.Client{Transport: alertRoundTripper(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		var event alertEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil || r.Header.Get("Idempotency-Key") != event.EventID ||
			r.Header.Get("Authorization") != "Bearer secret" {
			return nil, fmt.Errorf("invalid webhook request: %v", err)
		}
		code := http.StatusNoContent
		if len(events) == 0 {
			code = http.StatusServiceUnavailable
		}
		events <- event
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	t.Setenv("ALERT_TEST_TOKEN", "secret")
	cfg, err := loadAlertConfig(fmt.Sprintf(`{"enabled":true,"receivers":{"test":{"url":%q,"token_env":"ALERT_TEST_TOKEN"}}}`, "http://receiver.test/alerts"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alerts.db")
	open := func() *store {
		t.Helper()
		s, err := openStore(path)
		if err != nil {
			t.Fatal(err)
		}
		s.alerts, err = newAlertEngine(ctx, s, cfg)
		if err != nil {
			s.db.Close()
			t.Fatal(err)
		}
		return s
	}
	s := open()
	t.Cleanup(func() { s.db.Close() })
	now := time.Now()
	if err := s.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 30000}, now); err != nil {
		t.Fatal(err)
	}
	save := func(id int, state string) {
		t.Helper()
		now = now.Add(time.Second)
		snap := &protocol.Snapshot{MetricsJson: []byte(fmt.Sprintf(
			`{"sample_id":%d,"collected_at":%q,"processes":{"services":[{"key":"service/nginx","name":"nginx","state":%q}]}}`,
			id, now.Format(time.RFC3339Nano), state))}
		if err := s.save(ctx, "node", "run", snap, now); err != nil {
			t.Fatal(err)
		}
	}
	count := func(query string, expected int) {
		t.Helper()
		var n int
		if err := s.db.QueryRow(query).Scan(&n); err != nil || n != expected {
			t.Fatalf("%s: got %d, want %d, error %v", query, n, expected, err)
		}
	}
	save(1, "stopped")
	save(1, "stopped")
	count("SELECT count(*) FROM alert_events", 0)
	save(2, "stopped")
	count("SELECT count(*) FROM alert_events", 1)
	save(3, "unknown")
	save(4, "running")
	count("SELECT count(*) FROM alert_events", 1)
	save(5, "running")
	count("SELECT count(*) FROM alert_events", 2)

	deliver := func(at time.Time) {
		t.Helper()
		if processed, err := s.alerts.deliverOne(ctx, client, at); err != nil || !processed {
			t.Fatalf("deliver: processed=%t err=%v", processed, err)
		}
	}
	deliver(now)
	if processed, err := s.alerts.deliverOne(ctx, client, time.Now()); err != nil || processed {
		t.Fatalf("recovery overtook pending firing event: processed=%t err=%v", processed, err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	deliver(now.Add(time.Minute))
	deliver(now.Add(time.Minute))
	if len(events) != 3 {
		t.Fatalf("expected 3 webhook requests, got %d", len(events))
	}
	first, retry, recovered := <-events, <-events, <-events
	if first.Type != "FIRING" || first.EventID != retry.EventID || recovered.Type != "RESOLVED" ||
		recovered.AlertID != first.AlertID || recovered.Sequence <= first.Sequence {
		t.Fatalf("invalid event sequence: %+v %+v %+v", first, retry, recovered)
	}
	count("SELECT count(*) FROM alert_deliveries WHERE status='delivered'", 2)
}

func TestAlertOfflineRequiresGraceAndActualReconnect(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	cfg, err := loadAlertConfig(`{"enabled":true}`)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newAlertEngine(ctx, s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	host := hostRecord{ID: "node", LastSeenMS: now.Add(-time.Hour).UnixMilli()}
	check := func(after time.Duration, want int) {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := a.checkHost(ctx, tx, host, now, now.Add(after)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM alert_events").Scan(&count); err != nil || count != want {
			t.Fatalf("after %s: count=%d want=%d err=%v", after, count, want, err)
		}
	}
	check(30*time.Second, 0)
	check(65*time.Second, 1)
	host.Online, host.Collecting = true, "active"
	host.ConnectedMS = now.Add(70 * time.Second).UnixMilli()
	check(70*time.Second, 1)
	check(75*time.Second, 2)
}

type alertRoundTripper func(*http.Request) (*http.Response, error)

func (f alertRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestDiskAlertsIgnoreReadOnlyMounts(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	cfg, err := loadAlertConfig(`{"enabled":true,"rules":{"disk":{"trigger_count":1}}}`)
	if err != nil {
		t.Fatal(err)
	}
	s.alerts, err = newAlertEngine(ctx, s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 30000}, now); err != nil {
		t.Fatal(err)
	}
	snap := &protocol.Snapshot{
		MetricsJson:     []byte(fmt.Sprintf(`{"sample_id":1,"collected_at":%q}`, now.Format(time.RFC3339Nano))),
		FilesystemsJson: []byte(`{"mounts":[{"target":"/media/iso","read_only":true,"capacity":{"total_bytes":100,"used_bytes":100}},{"target":"/data","capacity":{"total_bytes":100,"used_bytes":95}}]}`),
	}
	if err := s.save(ctx, "node", "run", snap, now); err != nil {
		t.Fatal(err)
	}
	var count int
	var target string
	if err := s.db.QueryRow("SELECT count(*), max(target) FROM alert_instances WHERE rule_id='disk'").Scan(&count, &target); err != nil || count != 1 || target != "/data" {
		t.Fatalf("disk alerts: count=%d target=%q err=%v", count, target, err)
	}
}

func TestResolveDuringMuteOnlyNotifiesOriginalRecipient(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	cfg, err := loadAlertConfig(`{"enabled":true,"rules":{"service_stopped":{"trigger_count":1}},"receivers":{
		"received":{"url":"https://received.example/"},"missed":{"url":"https://missed.example/"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newAlertEngine(ctx, s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.alerts = a
	now := time.Now()
	if err := s.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 30000}, now); err != nil {
		t.Fatal(err)
	}
	save := func(id int, processes string) {
		t.Helper()
		at := now.Add(time.Duration(id) * time.Second)
		if err := s.save(ctx, "node", "run", &protocol.Snapshot{MetricsJson: []byte(fmt.Sprintf(
			`{"sample_id":%d,"collected_at":%q,"processes":%s}`, id, at.Format(time.RFC3339Nano), processes))}, at); err != nil {
			t.Fatal(err)
		}
	}
	save(1, `{"services":[{"key":"service/demo","state":"stopped"}]}`)
	if _, err := s.exec(ctx, "UPDATE alert_deliveries SET status=CASE WHEN receiver='received' THEN 'delivered' ELSE 'suppressed' END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, "UPDATE alert_instances SET mute_until_ms=?", now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	save(2, `{"issues":{"discovery":"temporarily unavailable"},"services":[]}`)
	var active int
	if err := s.db.QueryRow("SELECT count(*) FROM alert_instances WHERE active_key IS NOT NULL").Scan(&active); err != nil || active != 1 {
		t.Fatalf("partial discovery closed alert: active=%d err=%v", active, err)
	}
	save(3, `{"services":[]}`)
	calls := 0
	client := &http.Client{Transport: alertRoundTripper(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		calls++
		var event alertEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil ||
			event.Type != "RESOLVED" || event.Reason != "target_removed" || r.URL.Host != "received.example" {
			t.Errorf("unexpected recovery delivery: %s %+v %v", r.URL.Host, event, err)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}
	if processed, err := a.deliverOne(ctx, client, now.Add(time.Minute)); err != nil || !processed || calls != 1 {
		t.Fatalf("mute blocked recovery: processed=%v calls=%d err=%v", processed, calls, err)
	}
	if processed, err := a.deliverOne(ctx, client, now.Add(time.Minute)); err != nil || processed {
		t.Fatalf("unexpected extra recovery: processed=%v err=%v", processed, err)
	}
}

func TestStartupClosesAlertsOutsideRuleScope(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	cfg, err := loadAlertConfig(`{"enabled":true,"rules":{"demo":{"metric":"service_stopped","trigger_count":1}}}`)
	if err != nil {
		t.Fatal(err)
	}
	s.alerts, err = newAlertEngine(ctx, s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.hello(ctx, &protocol.Hello{HostId: "node", ReportIntervalMs: 30000}, now); err != nil {
		t.Fatal(err)
	}
	snap := &protocol.Snapshot{MetricsJson: []byte(fmt.Sprintf(
		`{"sample_id":1,"collected_at":%q,"processes":{"services":[{"key":"service/nginx","state":"stopped"}]}}`, now.Format(time.RFC3339Nano)))}
	if err := s.save(ctx, "node", "run", snap, now); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadAlertConfig(`{"enabled":true,"rules":{"service_stopped":{"enabled":false}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newAlertEngine(ctx, s, cfg); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := s.db.QueryRow("SELECT payload FROM alert_events WHERE sequence=2").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event alertEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil || event.Type != "RESOLVED" ||
		event.Reason != "rule_changed" || event.Metric != "service_stopped" {
		t.Fatalf("unexpected closing event: %+v err=%v", event, err)
	}
}
