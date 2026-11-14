package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type alertDelivery struct {
	EventID   string `json:"event_id"`
	Receiver  string `json:"receiver"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
	DueMS     int64  `json:"due_ms"`
	payload   string
	muteUntil int64
}

func (a *alertEngine) deliver(ctx context.Context) {
	// Do not forward notification credentials to a redirected destination.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil {
			processed, err := a.deliverOne(ctx, client, time.Now())
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("alert webhook queue: %v", err)
				}
				break
			}
			if !processed {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *alertEngine) deliverOne(ctx context.Context, client *http.Client, now time.Time) (bool, error) {
	var task alertDelivery
	var ruleID string
	err := a.db.db.QueryRowContext(ctx, a.db.bind(`
		SELECT d.event_id,d.receiver,d.status,d.due_ms,d.attempts,e.payload,i.mute_until_ms,i.rule_id
		FROM alert_deliveries d JOIN alert_events e ON e.id=d.event_id
		JOIN alert_instances i ON i.id=e.alert_id
		WHERE d.status IN ('pending','sending') AND d.due_ms<=?
		  AND NOT EXISTS (
			SELECT 1 FROM alert_events previous JOIN alert_deliveries p ON p.event_id=previous.id
			WHERE previous.alert_id=e.alert_id AND previous.sequence<e.sequence
			  AND p.receiver=d.receiver AND p.status IN ('pending','sending')
		  )
		ORDER BY d.due_ms,d.event_id,d.receiver LIMIT 1`), now.UnixMilli()).
		Scan(&task.EventID, &task.Receiver, &task.Status, &task.DueMS, &task.Attempts, &task.payload, &task.muteUntil, &ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	receiver, exists := a.cfg.Receivers[task.Receiver]
	lease := now.Add(2 * time.Minute).UnixMilli()
	claim, err := a.db.exec(ctx, `UPDATE alert_deliveries SET status='sending',due_ms=?
		WHERE event_id=? AND receiver=? AND status=? AND due_ms=?`,
		lease, task.EventID, task.Receiver, task.Status, task.DueMS)
	if err != nil {
		return false, err
	}
	count, err := claim.RowsAffected()
	if err != nil || count == 0 {
		return true, err
	}
	status, message, due := "failed", "", now
	var event alertEvent
	if err := json.Unmarshal([]byte(task.payload), &event); err != nil {
		return false, err
	}
	recovery := event.Type == "RESOLVED"
	firingDelivered := false
	if recovery {
		err := a.db.db.QueryRowContext(ctx, a.db.bind(`SELECT d.status FROM alert_events e JOIN alert_deliveries d ON d.event_id=e.id
			WHERE e.alert_id=? AND e.sequence=1 AND d.receiver=?`), event.AlertID, task.Receiver).Scan(&message)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		firingDelivered = message == "delivered"
		message = ""
	}
	enabled := false
	for _, rule := range a.cfg.Rules {
		if rule.ID == ruleID {
			enabled = rule.Enabled
			break
		}
	}
	switch {
	case recovery && !firingDelivered:
		status, message = "suppressed", "firing event was not delivered"
	case !recovery && !enabled:
		status, message = "suppressed", "rule is disabled"
	case !recovery && task.muteUntil > now.UnixMilli():
		status = "suppressed"
	case !exists:
		message = "receiver is no longer configured"
	default:
		task.Attempts++
		var retry bool
		message, retry = sendAlertWebhook(ctx, client, receiver, task)
		if ctx.Err() != nil {
			// Keep the lease so an interrupted delivery can be retried after restart.
			return false, ctx.Err()
		}
		if message == "" {
			status = "delivered"
		} else if retry && task.Attempts < receiver.MaxAttempts {
			status = "pending"
			due = time.Now().Add(time.Duration(5<<min(task.Attempts-1, 6)) * time.Second)
		}
	}
	_, err = a.db.exec(ctx, `UPDATE alert_deliveries SET status=?,due_ms=?,attempts=?,last_error=?
		WHERE event_id=? AND receiver=? AND status='sending' AND due_ms=?`,
		status, due.UnixMilli(), task.Attempts, message, task.EventID, task.Receiver, lease)
	return true, err
}

func sendAlertWebhook(ctx context.Context, client *http.Client, receiver alertReceiver, task alertDelivery) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, receiver.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, receiver.URL, strings.NewReader(task.payload))
	if err != nil {
		return "invalid webhook request", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", task.EventID)
	if receiver.token != "" {
		req.Header.Set("Authorization", "Bearer "+receiver.token)
	}
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "webhook timeout", true
		}
		return "webhook connection or TLS error", true
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return "", false
	}
	code := res.StatusCode
	return fmt.Sprintf("HTTP %d", code), code == 408 || code == 429 || code >= 500
}
