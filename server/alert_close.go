package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// closeInstance is idempotent and serializes against evaluation and operator actions.
func (a *alertEngine) closeInstance(ctx context.Context, tx *sql.Tx, id, reason string, now time.Time) (bool, error) {
	item := alertInstance{ID: id, reason: reason}
	err := tx.QueryRowContext(ctx, a.db.bind(`UPDATE alert_instances
		SET state='resolved',active_key=NULL,updated_ms=?,sequence=sequence+1
		WHERE id=? AND active_key IS NOT NULL
		RETURNING rule_id,host_id,target,value,sequence`), now.UnixMilli(), id).
		Scan(&item.Rule, &item.Host, &item.Target, &item.Value, &item.sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var payload string
	err = tx.QueryRowContext(ctx, a.db.bind("SELECT payload FROM alert_events WHERE alert_id=? AND sequence=1"), id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		// Pending instances have never emitted an event.
		_, err = a.db.execTx(tx, ctx, "DELETE FROM alert_instances WHERE id=?", id)
		return true, err
	}
	if err != nil {
		return false, err
	}
	var original alertEvent
	if err := json.Unmarshal([]byte(payload), &original); err != nil {
		return false, err
	}
	rule := alertRule{ID: item.Rule, Metric: original.Metric, Severity: original.Severity, Threshold: original.Threshold}
	return true, a.event(ctx, tx, rule, item, "RESOLVED", now)
}

func (a *alertEngine) closeOutOfScope(ctx context.Context, tx *sql.Tx, host, reason string, now time.Time) error {
	rules := make(map[string]alertRule, len(a.cfg.Rules))
	for _, rule := range a.cfg.Rules {
		rules[rule.ID] = rule
	}
	query := "SELECT id,rule_id,host_id,target FROM alert_instances WHERE active_key IS NOT NULL"
	var args []any
	if host != "" {
		query += " AND host_id=?"
		args = append(args, host)
	}
	rows, err := tx.QueryContext(ctx, a.db.bind(query), args...)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var item alertInstance
		if err := rows.Scan(&item.ID, &item.Rule, &item.Host, &item.Target); err != nil {
			rows.Close()
			return err
		}
		rule := rules[item.Rule]
		if host != "" || !rule.Enabled || (rule.Host != "" && rule.Host != item.Host) ||
			(rule.Target != "" && item.Target != rule.Target &&
				(rule.Metric != "service_stopped" || item.Target != "service/"+url.PathEscape(rule.Target))) {
			ids = append(ids, item.ID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := a.closeInstance(ctx, tx, id, reason, now); err != nil {
			return err
		}
	}
	return nil
}

func (a *alertEngine) closeAlert(w http.ResponseWriter, r *http.Request) {
	if !readAlertRequest(w, r, &struct{}{}) {
		return
	}
	ctx := r.Context()
	tx, err := a.db.db.BeginTx(ctx, nil)
	if err != nil {
		storageError(w, err)
		return
	}
	defer tx.Rollback()
	closed, err := a.closeInstance(ctx, tx, r.PathValue("id"), "manual", time.Now())
	if err != nil {
		storageError(w, err)
		return
	}
	if !closed {
		writeError(w, http.StatusNotFound, "active alert not found")
		return
	}
	if err := tx.Commit(); err != nil {
		storageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
