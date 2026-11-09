package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"monitor/protocol"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type store struct {
	db       *sql.DB
	postgres bool
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
CREATE TABLE IF NOT EXISTS hosts (
  host_id TEXT PRIMARY KEY, last_seen_ms INTEGER NOT NULL, report_interval_ms INTEGER NOT NULL,
  inventory_version TEXT NOT NULL DEFAULT '', inventory_ms INTEGER NOT NULL DEFAULT 0,
  run_id TEXT NOT NULL DEFAULT '', sample_id INTEGER NOT NULL DEFAULT 0,
  latest_ms INTEGER NOT NULL DEFAULT 0, metrics BLOB, filesystems BLOB
);
CREATE TABLE IF NOT EXISTS inventories (
  host_id TEXT NOT NULL REFERENCES hosts(host_id), version TEXT NOT NULL, payload BLOB NOT NULL,
  PRIMARY KEY(host_id, version)
);
CREATE TABLE IF NOT EXISTS samples (
  host_id TEXT NOT NULL REFERENCES hosts(host_id), run_id TEXT NOT NULL,
  sample_id INTEGER NOT NULL, collected_at TEXT NOT NULL, received_ms INTEGER NOT NULL,
  inventory_version TEXT NOT NULL, metrics BLOB NOT NULL, filesystems BLOB,
  PRIMARY KEY(host_id, run_id, sample_id)
);
CREATE INDEX IF NOT EXISTS samples_by_host_order ON samples(host_id, received_ms DESC, collected_at DESC, run_id DESC, sample_id DESC);
CREATE INDEX IF NOT EXISTS samples_by_time ON samples(received_ms);
`

func openStore(path string) (*store, error) { return openDatabase("sqlite", path) }

func openDatabase(driver, dsn string) (*store, error) {
	pg := driver == "postgres"
	if driver != "sqlite" && !pg {
		return nil, errors.New("db-driver must be sqlite or postgres")
	}
	sqlDriver := driver
	if pg {
		sqlDriver = "pgx"
	}
	db, err := sql.Open(sqlDriver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if pg {
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(30 * time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ddl := schema
	if pg {
		ddl = ddl[strings.Index(ddl, "CREATE TABLE"):]
		ddl = strings.ReplaceAll(strings.ReplaceAll(ddl, "INTEGER", "BIGINT"), "BLOB", "BYTEA")
	}
	// Execute statements separately so PostgreSQL need not accept multiple prepared statements.
	for _, statement := range strings.Split(ddl, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &store{db: db, postgres: pg}, nil
}

func (s *store) bind(query string) string {
	if !s.postgres {
		return query
	}
	var out strings.Builder
	n := 0
	for _, ch := range query {
		if ch == '?' {
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
		} else {
			out.WriteRune(ch)
		}
	}
	return out.String()
}
func (s *store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.bind(query), args...)
}
func (s *store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.bind(query), args...)
}
func (s *store) execTx(tx *sql.Tx, ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, s.bind(query), args...)
}

func (s *store) hello(ctx context.Context, h *protocol.Hello, now time.Time) error {
	_, err := s.exec(ctx, `INSERT INTO hosts(host_id,last_seen_ms,report_interval_ms) VALUES(?,?,?)
		ON CONFLICT(host_id) DO UPDATE SET last_seen_ms=excluded.last_seen_ms, report_interval_ms=excluded.report_interval_ms`,
		h.HostId, now.UnixMilli(), h.ReportIntervalMs)
	return err
}

func (s *store) seen(ctx context.Context, id string, now time.Time) error {
	_, err := s.exec(ctx, "UPDATE hosts SET last_seen_ms=? WHERE host_id=?", now.UnixMilli(), id)
	return err
}

type metricHeader struct {
	SampleID    int64     `json:"sample_id"`
	CollectedAt time.Time `json:"collected_at"`
}

func validateSnapshot(id string, snap *protocol.Snapshot) (metricHeader, string, time.Time, error) {
	var metric metricHeader
	var inventoryAt time.Time
	var version string
	if len(snap.GetMetricsJson()) > 0 {
		if err := json.Unmarshal(snap.MetricsJson, &metric); err != nil || metric.SampleID <= 0 || metric.CollectedAt.IsZero() {
			return metric, "", inventoryAt, errors.New("invalid metric sample")
		}
	}
	if len(snap.GetFilesystemsJson()) > 0 && !json.Valid(snap.FilesystemsJson) {
		return metric, "", inventoryAt, errors.New("invalid filesystem data")
	}
	if len(snap.GetInventoryJson()) > 0 {
		var header struct {
			ID string    `json:"host_id"`
			At time.Time `json:"collected_at"`
		}
		if err := json.Unmarshal(snap.InventoryJson, &header); err != nil || header.ID != id || header.At.IsZero() {
			return metric, "", inventoryAt, errors.New("invalid inventory or host mismatch")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(snap.InventoryJson, &fields); err != nil {
			return metric, "", inventoryAt, err
		}
		delete(fields, "collected_at")
		data, err := json.Marshal(fields)
		if err != nil {
			return metric, "", inventoryAt, err
		}
		hash := sha256.Sum256(data)
		version = hex.EncodeToString(hash[:])
		inventoryAt = header.At
	}
	if metric.SampleID == 0 && version == "" {
		return metric, "", inventoryAt, errors.New("empty snapshot")
	}
	return metric, version, inventoryAt, nil
}

func (s *store) save(ctx context.Context, id, run string, snap *protocol.Snapshot, now time.Time) error {
	metric, version, inventoryAt, err := validateSnapshot(id, snap)
	if err != nil {
		return err
	}
	fsPayload := snap.FilesystemsJson
	if len(fsPayload) == 0 {
		fsPayload = nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version != "" {
		if _, err := s.execTx(tx, ctx, "INSERT INTO inventories VALUES(?,?,?) ON CONFLICT(host_id,version) DO NOTHING", id, version, snap.InventoryJson); err != nil {
			return err
		}
		if _, err := s.execTx(tx, ctx, "UPDATE hosts SET inventory_version=?,inventory_ms=? WHERE host_id=?",
			version, inventoryAt.UnixMilli(), id); err != nil {
			return err
		}
	}
	if metric.SampleID > 0 {
		res, err := s.execTx(tx, ctx, `INSERT INTO samples
			SELECT host_id, ?, ?, ?, ?, inventory_version, ?, ? FROM hosts WHERE host_id=? ON CONFLICT(host_id,run_id,sample_id) DO NOTHING`,
			run, metric.SampleID, metric.CollectedAt.UTC().Format(time.RFC3339Nano), now.UnixMilli(), snap.MetricsJson, fsPayload, id)
		if err != nil {
			return err
		}
		if count, err := res.RowsAffected(); err != nil {
			return err
		} else if count > 0 {
			if _, err := s.execTx(tx, ctx, `UPDATE hosts SET run_id=?, sample_id=?, latest_ms=?, metrics=?, filesystems=COALESCE(?,filesystems)
				WHERE host_id=? AND (run_id<>? OR sample_id<?)`, run, metric.SampleID, now.UnixMilli(),
				snap.MetricsJson, fsPayload, id, run, metric.SampleID); err != nil {
				return err
			}
		}
	}
	if _, err := s.execTx(tx, ctx, "UPDATE hosts SET last_seen_ms=? WHERE host_id=?", now.UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}

type hostRecord struct {
	ID               string          `json:"host_id"`
	LastSeenMS       int64           `json:"last_seen_ms"`
	ReportIntervalMS int64           `json:"report_interval_ms"`
	InventoryMS      int64           `json:"inventory_ms,omitempty"`
	InventoryVersion string          `json:"inventory_version,omitempty"`
	LatestMS         int64           `json:"latest_received_ms,omitempty"`
	Inventory        json.RawMessage `json:"inventory,omitempty"`
	Metrics          json.RawMessage `json:"metrics,omitempty"`
	Filesystems      json.RawMessage `json:"filesystems,omitempty"`
	Online           bool            `json:"online"`
	Collecting       string          `json:"collection_status"`
	ConnectedMS      int64           `json:"connected_ms,omitempty"`
}

func (s *store) hosts(ctx context.Context, id string) ([]hostRecord, error) {
	query := `SELECT h.host_id,h.last_seen_ms,h.report_interval_ms,h.inventory_ms,h.inventory_version,
		h.latest_ms,i.payload,h.metrics,h.filesystems FROM hosts h LEFT JOIN inventories i
		ON i.host_id=h.host_id AND i.version=h.inventory_version`
	var args []any
	if id != "" {
		query += " WHERE h.host_id=?"
		args = append(args, id)
	}
	rows, err := s.query(ctx, query+" ORDER BY h.host_id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]hostRecord, 0)
	for rows.Next() {
		var h hostRecord
		var inventory, metrics, fs []byte
		if err := rows.Scan(&h.ID, &h.LastSeenMS, &h.ReportIntervalMS, &h.InventoryMS,
			&h.InventoryVersion, &h.LatestMS, &inventory, &metrics, &fs); err != nil {
			return nil, err
		}
		h.Inventory, h.Metrics, h.Filesystems = inventory, metrics, fs
		records = append(records, h)
	}
	return records, rows.Err()
}

type historyPoint struct {
	Services         json.RawMessage               `json:"services,omitempty"`
	ServiceStatus    map[string]map[string]string  `json:"service_status,omitempty"`
	CollectedAt      string                        `json:"collected_at"`
	ReceivedMS       int64                         `json:"received_ms"`
	CPU              *float64                      `json:"cpu_percent"`
	Memory           *float64                      `json:"memory_percent"`
	Swap             *float64                      `json:"swap_percent"`
	WindowMS         float64                       `json:"window_ms"`
	ActualWindowMS   float64                       `json:"elapsed_ms"`
	Rates            map[string]map[string]float64 `json:"rates,omitempty"`
	Filesystems      json.RawMessage               `json:"filesystems,omitempty"`
	InventoryVersion string                        `json:"inventory_version,omitempty"`
}

func (s *store) history(ctx context.Context, id string, since time.Time) ([]historyPoint, error) {
	rows, err := s.query(ctx, `SELECT collected_at,received_ms,inventory_version,metrics,filesystems FROM samples
		WHERE host_id=? AND received_ms>=? ORDER BY received_ms DESC, collected_at DESC, run_id DESC, sample_id DESC LIMIT 3000`, id, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make([]historyPoint, 0)
	for rows.Next() {
		var p historyPoint
		var metrics, fs []byte
		if err := rows.Scan(&p.CollectedAt, &p.ReceivedMS, &p.InventoryVersion, &metrics, &fs); err != nil {
			return nil, err
		}
		var sample struct {
			Processes struct {
				Services json.RawMessage `json:"services"`
			} `json:"processes"`
			Memory  map[string]uint64 `json:"memory_bytes"`
			Windows []struct {
				ServiceStatus map[string]map[string]string  `json:"service_status"`
				Requested     float64                       `json:"requested_ms"`
				Elapsed       float64                       `json:"elapsed_ms"`
				CPU           map[string]map[string]float64 `json:"cpu_percent"`
				Rates         map[string]map[string]float64 `json:"rates"`
			} `json:"windows"`
		}
		if err := json.Unmarshal(metrics, &sample); err != nil {
			return nil, fmt.Errorf("stored metrics: %w", err)
		}
		if len(sample.Windows) > 0 {
			w := sample.Windows[0]
			p.ServiceStatus = w.ServiceStatus
			p.WindowMS, p.ActualWindowMS, p.Rates = w.Requested, w.Elapsed, w.Rates
			if n, ok := w.CPU["cpu"]["busy"]; ok {
				p.CPU = &n
			}
		}
		p.Services = sample.Processes.Services
		p.Memory = utilization(sample.Memory, "used", "total")
		p.Swap = utilization(sample.Memory, "swap_used", "swap_total")
		p.Filesystems = fs
		points = append(points, p)
	}
	for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
		points[i], points[j] = points[j], points[i]
	}
	return points, rows.Err()
}

func utilization(metrics map[string]uint64, used, total string) *float64 {
	n, ok := metrics[used]
	if !ok || metrics[total] == 0 {
		return nil
	}
	pct := 100 * float64(n) / float64(metrics[total])
	return &pct
}

func (s *store) prune(ctx context.Context, before time.Time) error {
	_, err := s.exec(ctx, "DELETE FROM samples WHERE received_ms < ?", before.UnixMilli())
	return err
}
