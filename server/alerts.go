package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

const alertSchema = `
CREATE TABLE IF NOT EXISTS alert_instances (
  id TEXT PRIMARY KEY, active_key TEXT UNIQUE, rule_id TEXT NOT NULL, rule_version TEXT NOT NULL,
  host_id TEXT NOT NULL, target TEXT NOT NULL, state TEXT NOT NULL, hits INTEGER NOT NULL, clears INTEGER NOT NULL,
  last_observation BIGINT NOT NULL, created_ms BIGINT NOT NULL, updated_ms BIGINT NOT NULL,
  value DOUBLE PRECISION, sequence INTEGER NOT NULL DEFAULT 0, ack_ms BIGINT NOT NULL DEFAULT 0,
  mute_until_ms BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS alert_instances_scope ON alert_instances(host_id,rule_id,active_key);
CREATE INDEX IF NOT EXISTS alert_instances_recent ON alert_instances(updated_ms);
CREATE TABLE IF NOT EXISTS alert_events (
  id TEXT PRIMARY KEY, alert_id TEXT NOT NULL REFERENCES alert_instances(id), sequence INTEGER NOT NULL,
  occurred_ms BIGINT NOT NULL, payload TEXT NOT NULL, UNIQUE(alert_id,sequence)
);
CREATE INDEX IF NOT EXISTS alert_events_recent ON alert_events(occurred_ms);
CREATE TABLE IF NOT EXISTS alert_deliveries (
  event_id TEXT NOT NULL REFERENCES alert_events(id), receiver TEXT NOT NULL,
  status TEXT NOT NULL, due_ms BIGINT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '', PRIMARY KEY(event_id,receiver)
);
CREATE INDEX IF NOT EXISTS alert_deliveries_due ON alert_deliveries(status,due_ms);
`

type alertEngine struct {
	db  *store
	cfg alertConfig
}

func newAlertEngine(ctx context.Context, db *store, cfg alertConfig) (*alertEngine, error) {
	for _, ddl := range strings.Split(alertSchema, ";") {
		if strings.TrimSpace(ddl) == "" {
			continue
		}
		if _, err := db.exec(ctx, ddl); err != nil {
			return nil, fmt.Errorf("initialize alerts: %w", err)
		}
	}
	a := &alertEngine{db: db, cfg: cfg}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := a.closeOutOfScope(ctx, tx, "", "rule_changed", time.Now()); err != nil {
		return nil, err
	}
	return a, tx.Commit()
}

type alertEvent struct {
	EventID    string    `json:"event_id"`
	AlertID    string    `json:"alert_id"`
	Sequence   int       `json:"sequence"`
	Type       string    `json:"type"`
	Rule       string    `json:"rule_id"`
	Metric     string    `json:"metric"`
	Severity   string    `json:"severity"`
	Host       string    `json:"host_id"`
	Target     string    `json:"target,omitempty"`
	Value      *float64  `json:"value"`
	Threshold  float64   `json:"threshold"`
	OccurredAt time.Time `json:"occurred_at"`
	Reason     string    `json:"reason,omitempty"`
}

type alertInstance struct {
	ID          string   `json:"alert_id"`
	Rule        string   `json:"rule_id"`
	Host        string   `json:"host_id"`
	Target      string   `json:"target"`
	State       string   `json:"state"`
	Value       *float64 `json:"value"`
	UpdatedMS   int64    `json:"updated_ms"`
	AckMS       int64    `json:"ack_ms"`
	MuteUntilMS int64    `json:"mute_until_ms"`
	version     string
	hits        int
	clears      int
	last        int64
	sequence    int
	reason      string
}

func alertID() string {
	var value [16]byte
	rand.Read(value[:])
	return hex.EncodeToString(value[:])
}

func alertKey(fields ...string) string {
	data, _ := json.Marshal(fields)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (a *alertEngine) event(ctx context.Context, tx *sql.Tx, rule alertRule, instance alertInstance, kind string, now time.Time) error {
	event := alertEvent{EventID: alertID(), AlertID: instance.ID, Sequence: instance.sequence, Type: kind,
		Rule: rule.ID, Metric: rule.Metric, Severity: rule.Severity, Host: instance.Host, Target: instance.Target,
		Value: instance.Value, Threshold: rule.Threshold, OccurredAt: now.UTC(), Reason: instance.reason}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := a.db.execTx(tx, ctx, "INSERT INTO alert_events(id,alert_id,sequence,occurred_ms,payload) VALUES(?,?,?,?,?)",
		event.EventID, event.AlertID, event.Sequence, now.UnixMilli(), string(payload)); err != nil {
		return err
	}
	if kind == "RESOLVED" {
		// Use original recipient history, including a firing event still awaiting delivery.
		_, err := a.db.execTx(tx, ctx, `INSERT INTO alert_deliveries(event_id,receiver,status,due_ms)
			SELECT ?,d.receiver,'pending',? FROM alert_events e JOIN alert_deliveries d ON d.event_id=e.id
			WHERE e.alert_id=? AND e.sequence=1 AND d.status IN ('delivered','pending','sending')`,
			event.EventID, now.UnixMilli(), instance.ID)
		return err
	}
	if kind != "FIRING" {
		return nil
	}
	for name := range a.cfg.Receivers {
		if rule.Receivers != nil && !slices.Contains(rule.Receivers, name) {
			continue
		}
		status := "pending"
		if instance.MuteUntilMS > now.UnixMilli() {
			status = "suppressed"
		}
		if _, err := a.db.execTx(tx, ctx, "INSERT INTO alert_deliveries(event_id,receiver,status,due_ms) VALUES(?,?,?,?)",
			event.EventID, name, status, now.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

func (a *alertEngine) observe(ctx context.Context, tx *sql.Tx, rule alertRule, host, target string, value *float64, bad bool, observation int64, now time.Time, maxGap time.Duration) error {
	key := alertKey(rule.ID, host, target)
	// Changing evaluation settings resets counters while preserving a firing incident.
	version := alertRuleVersion(rule)
	item := alertInstance{Rule: rule.ID, Host: host, Target: target}
	query := "SELECT id,state,rule_version,hits,clears,last_observation,sequence,mute_until_ms,updated_ms FROM alert_instances WHERE active_key=?"
	if a.db.postgres {
		query += " FOR UPDATE"
	}
	err := tx.QueryRowContext(ctx, a.db.bind(query), key).Scan(&item.ID, &item.State, &item.version, &item.hits,
		&item.clears, &item.last, &item.sequence, &item.MuteUntilMS, &item.UpdatedMS)
	if errors.Is(err, sql.ErrNoRows) {
		if value == nil || !bad {
			return nil
		}
		item.ID, item.State = alertID(), "pending"
		if _, err := a.db.execTx(tx, ctx, `INSERT INTO alert_instances
			(id,active_key,rule_id,rule_version,host_id,target,state,hits,clears,last_observation,created_ms,updated_ms)
			VALUES(?,?,?,?,?,?,?,0,0,0,?,?)`, item.ID, key, rule.ID, version, host, target, item.State, now.UnixMilli(), now.UnixMilli()); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if observation <= item.last && item.version == version {
		return nil
	}
	if item.version != version || (maxGap > 0 && now.UnixMilli()-item.UpdatedMS > maxGap.Milliseconds()) {
		item.hits, item.clears = 0, 0
	}
	item.Value = value
	kind := ""
	var activeKey any = key
	switch {
	case value == nil:
		if item.State == "pending" {
			_, err := a.db.execTx(tx, ctx, "DELETE FROM alert_instances WHERE id=?", item.ID)
			return err
		}
		item.hits, item.clears = 0, 0
	case bad:
		item.hits++
		item.clears = 0
		if item.State == "pending" && item.hits >= rule.Trigger {
			item.State, kind = "firing", "FIRING"
		}
	default:
		item.hits = 0
		if item.State == "pending" {
			_, err := a.db.execTx(tx, ctx, "DELETE FROM alert_instances WHERE id=?", item.ID)
			return err
		}
		item.clears++
		if item.clears >= rule.Recover {
			item.State, kind, activeKey = "resolved", "RESOLVED", nil
		}
	}
	if kind != "" {
		item.sequence++
	}
	if _, err := a.db.execTx(tx, ctx, `UPDATE alert_instances SET state=?,active_key=?,rule_version=?,hits=?,clears=?,
		last_observation=?,updated_ms=?,value=?,sequence=? WHERE id=?`, item.State, activeKey, version, item.hits,
		item.clears, observation, now.UnixMilli(), value, item.sequence, item.ID); err != nil {
		return err
	}
	if kind != "" {
		return a.event(ctx, tx, rule, item, kind, now)
	}
	return nil
}

func (a *alertEngine) tick(ctx context.Context, h *hub, started, now time.Time) error {
	rows, err := a.db.query(ctx, "SELECT host_id,last_seen_ms,latest_ms,report_interval_ms FROM hosts")
	if err != nil {
		return err
	}
	var hosts []hostRecord
	for rows.Next() {
		var host hostRecord
		if err := rows.Scan(&host.ID, &host.LastSeenMS, &host.LatestMS, &host.ReportIntervalMS); err != nil {
			rows.Close()
			return err
		}
		hosts = append(hosts, host)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, host := range hosts {
		h.state(&host, now)
		tx, err := a.db.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = a.checkKnownHost(ctx, tx, host, started, now)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (a *alertEngine) checkKnownHost(ctx context.Context, tx *sql.Tx, host hostRecord, started, now time.Time) error {
	// A host can be retired after the liveness scan took its snapshot.
	res, err := a.db.execTx(tx, ctx, "UPDATE hosts SET last_seen_ms=last_seen_ms WHERE host_id=? AND last_seen_ms<=?", host.ID, host.LastSeenMS)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	return a.checkHost(ctx, tx, host, started, now)
}

func (a *alertEngine) checkHost(ctx context.Context, tx *sql.Tx, host hostRecord, started, now time.Time) error {
	for _, rule := range a.cfg.Rules {
		if !rule.Enabled || (rule.Host != "" && rule.Host != host.ID) {
			continue
		}
		var value float64
		var bad bool
		switch rule.Metric {
		case "host_offline":
			lastSeen := max(host.LastSeenMS, started.UnixMilli())
			if !host.Online {
				value = float64(now.UnixMilli()-lastSeen) / 1000
				bad = value >= rule.Threshold
				if !bad {
					continue // A reconnect grace period is not evidence of recovery.
				}
			}
		case "collection_stalled":
			if !host.Online {
				continue
			}
			if host.Collecting != "active" {
				value = float64(now.UnixMilli()-max(host.ProgressMS, host.ConnectedMS)) / 1000
				bad = value >= rule.Threshold
				if !bad {
					continue
				}
			}
		case "snapshot_missing":
			if !host.Online {
				continue
			}
			value = float64(now.UnixMilli()-max(host.LatestMS, host.ConnectedMS)) / 1000
			bad = value >= max(rule.Threshold, 3*float64(host.ReportIntervalMS)/1000)
			if !bad && host.LatestMS < host.ConnectedMS {
				continue
			}
		default:
			continue
		}
		if err := a.observe(ctx, tx, rule, host.ID, "", &value, bad, now.UnixNano(), now, 15*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func (a *alertEngine) run(ctx context.Context, h *hub) {
	started := time.Now()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := a.tick(ctx, h, started, now); err != nil && ctx.Err() == nil {
				log.Printf("alert liveness evaluation: %v", err)
			}
		}
	}
}
