package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

func (h *hub) alertRoutes(mux *http.ServeMux) {
	a := h.store.alerts
	if a == nil {
		return
	}
	mux.HandleFunc("GET /api/alerts", a.list)
	mux.HandleFunc("GET /api/alerts/events", a.listEvents)
	mux.HandleFunc("POST /api/alerts/{id}/ack", a.action)
	mux.HandleFunc("POST /api/alerts/{id}/mute", a.action)
	mux.HandleFunc("POST /api/alerts/{id}/resolve", a.closeAlert)
	mux.HandleFunc("POST /api/alerts/events/{id}/retry", a.retry)
}

func (a *alertEngine) list(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.query(r.Context(), `SELECT id,rule_id,host_id,target,state,value,updated_ms,ack_ms,mute_until_ms
		FROM alert_instances WHERE active_key IS NOT NULL ORDER BY updated_ms DESC,id LIMIT 500`)
	if err != nil {
		storageError(w, err)
		return
	}
	defer rows.Close()
	items := make([]alertInstance, 0)
	for rows.Next() {
		var item alertInstance
		if err := rows.Scan(&item.ID, &item.Rule, &item.Host, &item.Target, &item.State, &item.Value,
			&item.UpdatedMS, &item.AckMS, &item.MuteUntilMS); err != nil {
			storageError(w, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		storageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": a.cfg.Enabled, "rules": a.cfg.Rules, "active": items})
}

type alertEventView struct {
	alertEvent
	Deliveries []alertDelivery `json:"deliveries"`
}

func (a *alertEngine) listEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.query(r.Context(), `SELECT recent.payload,d.receiver,d.status,d.attempts,d.last_error,d.due_ms
		FROM (SELECT id,payload,occurred_ms FROM alert_events ORDER BY occurred_ms DESC,id DESC LIMIT 200) recent
		LEFT JOIN alert_deliveries d ON d.event_id=recent.id ORDER BY recent.occurred_ms DESC,recent.id DESC,d.receiver`)
	if err != nil {
		storageError(w, err)
		return
	}
	defer rows.Close()
	events := make([]alertEventView, 0)
	for rows.Next() {
		var payload string
		var receiver, status, message sql.NullString
		var attempts, due sql.NullInt64
		if err := rows.Scan(&payload, &receiver, &status, &attempts, &message, &due); err != nil {
			storageError(w, err)
			return
		}
		var event alertEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			storageError(w, err)
			return
		}
		if len(events) == 0 || events[len(events)-1].EventID != event.EventID {
			events = append(events, alertEventView{event, []alertDelivery{}})
		}
		if receiver.Valid {
			last := &events[len(events)-1]
			last.Deliveries = append(last.Deliveries, alertDelivery{EventID: event.EventID, Receiver: receiver.String,
				Status: status.String, Attempts: int(attempts.Int64), LastError: message.String, DueMS: due.Int64})
		}
	}
	if err := rows.Err(); err != nil {
		storageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (a *alertEngine) action(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Seconds int `json:"seconds"`
	}
	if !readAlertRequest(w, r, &input) {
		return
	}
	kind := "ACK"
	now := time.Now()
	query := "UPDATE alert_instances SET ack_ms=?, sequence=sequence+1"
	value := now.UnixMilli()
	if r.Pattern == "POST /api/alerts/{id}/mute" {
		if input.Seconds < 1 || input.Seconds > 7*86400 {
			writeError(w, http.StatusBadRequest, "mute duration must be 1–604800 seconds")
			return
		}
		kind = "MUTED"
		query = "UPDATE alert_instances SET mute_until_ms=?, sequence=sequence+1"
		value = now.Add(time.Duration(input.Seconds) * time.Second).UnixMilli()
	}
	ctx := r.Context()
	tx, err := a.db.db.BeginTx(ctx, nil)
	if err != nil {
		storageError(w, err)
		return
	}
	defer tx.Rollback()
	item := alertInstance{ID: r.PathValue("id")}
	err = tx.QueryRowContext(ctx, a.db.bind(query+` WHERE id=? AND state='firing'
		RETURNING rule_id,host_id,target,value,sequence,mute_until_ms`), value, item.ID).
		Scan(&item.Rule, &item.Host, &item.Target, &item.Value, &item.sequence, &item.MuteUntilMS)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "active alert not found")
		return
	}
	if err != nil {
		storageError(w, err)
		return
	}
	rule := alertRule{ID: item.Rule}
	for _, current := range a.cfg.Rules {
		if current.ID == item.Rule {
			rule = current
			break
		}
	}
	if err := a.event(ctx, tx, rule, item, kind, now); err != nil {
		storageError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		storageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *alertEngine) retry(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Receiver string `json:"receiver"`
	}
	if !readAlertRequest(w, r, &input) {
		return
	}
	if !a.cfg.Enabled {
		writeError(w, http.StatusConflict, "alerts are disabled")
		return
	}
	if _, ok := a.cfg.Receivers[input.Receiver]; !ok {
		writeError(w, http.StatusBadRequest, "unknown receiver")
		return
	}
	ctx := r.Context()
	tx, err := a.db.db.BeginTx(ctx, nil)
	if err != nil {
		storageError(w, err)
		return
	}
	defer tx.Rollback()
	// Share the incident lock with retention so an accepted retry cannot be pruned.
	lock, err := a.db.execTx(tx, ctx, `UPDATE alert_instances SET id=id
		WHERE id=(SELECT alert_id FROM alert_events WHERE id=?)`, r.PathValue("id"))
	if err != nil {
		storageError(w, err)
		return
	}
	if n, err := lock.RowsAffected(); err != nil {
		storageError(w, err)
		return
	} else if n == 0 {
		writeError(w, http.StatusNotFound, "alert event not found")
		return
	}
	var payload, state string
	if err := tx.QueryRowContext(ctx, a.db.bind(`SELECT e.payload,i.state FROM alert_events e
		JOIN alert_instances i ON i.id=e.alert_id WHERE e.id=?`), r.PathValue("id")).Scan(&payload, &state); err != nil {
		storageError(w, err)
		return
	}
	var event alertEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		storageError(w, err)
		return
	}
	if event.Type == "FIRING" && state == "resolved" {
		writeError(w, http.StatusConflict, "cannot resend a firing event for a resolved alert")
		return
	}
	res, err := a.db.execTx(tx, ctx, `UPDATE alert_deliveries SET status='pending',due_ms=?,attempts=0,last_error=''
		WHERE event_id=? AND receiver=? AND status='failed'`, time.Now().UnixMilli(), r.PathValue("id"), input.Receiver)
	if err != nil {
		storageError(w, err)
		return
	}
	count, err := res.RowsAffected()
	if err != nil {
		storageError(w, err)
		return
	}
	if count == 0 {
		writeError(w, http.StatusConflict, "delivery is not failed")
		return
	}
	if err := tx.Commit(); err != nil {
		storageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func readAlertRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json required")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid alert request")
		return false
	}
	return true
}
