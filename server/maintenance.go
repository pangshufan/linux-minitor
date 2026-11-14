package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"monitor/internal/transport"
)

func (s *store) prune(ctx context.Context, before time.Time) error {
	if _, err := s.exec(ctx, "DELETE FROM samples WHERE received_ms < ?", before.UnixMilli()); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.postgres {
		// A snapshot can begin referencing an existing version while pruning runs.
		if _, err := tx.ExecContext(ctx, "LOCK TABLE hosts IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM inventories
		WHERE NOT EXISTS (SELECT 1 FROM hosts h WHERE h.host_id=inventories.host_id AND h.inventory_version=inventories.version)
		  AND NOT EXISTS (SELECT 1 FROM samples s WHERE s.host_id=inventories.host_id AND s.inventory_version=inventories.version)`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.alerts != nil {
		return s.alerts.prune(ctx, before)
	}
	return nil
}

func (a *alertEngine) prune(ctx context.Context, before time.Time) error {
	for {
		n, err := a.pruneBatch(ctx, before)
		if err != nil || n == 0 {
			return err
		}
	}
}

func (a *alertEngine) pruneBatch(ctx context.Context, before time.Time) (int, error) {
	tx, err := a.db.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	query := `SELECT i.id FROM alert_instances i
		WHERE i.state='resolved' AND i.active_key IS NULL AND i.updated_ms<?
		  AND NOT EXISTS (SELECT 1 FROM alert_events e JOIN alert_deliveries d ON d.event_id=e.id
		    WHERE e.alert_id=i.id AND d.status IN ('pending','sending'))
		ORDER BY i.updated_ms,i.id LIMIT 200`
	if a.db.postgres {
		query += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, a.db.bind(query), before.UnixMilli())
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		// Recheck after taking the instance lock. Manual retry uses that same lock.
		var pending int
		if err := tx.QueryRowContext(ctx, a.db.bind(`SELECT count(*) FROM alert_events e
			JOIN alert_deliveries d ON d.event_id=e.id WHERE e.alert_id=? AND d.status IN ('pending','sending')`), id).Scan(&pending); err != nil {
			return 0, err
		}
		if pending != 0 {
			continue
		}
		for _, statement := range []string{
			"DELETE FROM alert_deliveries WHERE event_id IN (SELECT id FROM alert_events WHERE alert_id=?)",
			"DELETE FROM alert_events WHERE alert_id=?",
			"DELETE FROM alert_instances WHERE id=?",
		} {
			if _, err := a.db.execTx(tx, ctx, statement, id); err != nil {
				return 0, err
			}
		}
	}
	return len(ids), tx.Commit()
}

var errHostNotFound = errors.New("host not found")

func (s *store) retire(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := s.execTx(tx, ctx, "UPDATE hosts SET last_seen_ms=last_seen_ms WHERE host_id=?", id)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return errHostNotFound
	}
	if s.alerts != nil {
		if err := s.alerts.closeOutOfScope(ctx, tx, id, "host_retired", now); err != nil {
			return err
		}
	}
	for _, statement := range []string{"DELETE FROM samples WHERE host_id=?", "DELETE FROM inventories WHERE host_id=?"} {
		if _, err := s.execTx(tx, ctx, statement, id); err != nil {
			return err
		}
	}
	if _, err := s.execTx(tx, ctx, "DELETE FROM hosts WHERE host_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *hub) retireHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !transport.ValidHostID(id) {
		writeError(w, http.StatusBadRequest, "invalid host ID")
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json required")
		return
	}
	h.mu.Lock()
	if h.sessions[id] != nil {
		h.mu.Unlock()
		writeError(w, http.StatusConflict, "disconnect the Agent before retiring the host")
		return
	}
	err := h.store.retire(r.Context(), id, time.Now())
	h.mu.Unlock()
	switch {
	case errors.Is(err, errHostNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		storageError(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
