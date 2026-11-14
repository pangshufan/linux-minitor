package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"monitor/protocol"
)

type alertSample struct {
	Memory  map[string]uint64 `json:"memory_bytes"`
	Windows []struct {
		RequestedMS float64                       `json:"requested_ms"`
		Status      string                        `json:"status"`
		CPU         map[string]map[string]float64 `json:"cpu_percent"`
	} `json:"windows"`
	Processes struct {
		Enabled  *bool             `json:"enabled"`
		Issues   map[string]string `json:"issues"`
		Services []struct {
			Key   string `json:"key"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"services"`
	} `json:"processes"`
}

func (a *alertEngine) sample(ctx context.Context, tx *sql.Tx, host string, snap *protocol.Snapshot, now time.Time) error {
	var sample alertSample
	var filesystems struct {
		Issue  string `json:"issue"`
		Mounts []struct {
			Target   string            `json:"target"`
			ReadOnly bool              `json:"read_only"`
			Capacity map[string]uint64 `json:"capacity"`
		} `json:"mounts"`
	}
	// Invalid or missing measurements remain unknown. The monitoring snapshot is still useful.
	if json.Unmarshal(snap.MetricsJson, &sample) != nil {
		sample = alertSample{}
	}
	if json.Unmarshal(snap.FilesystemsJson, &filesystems) != nil {
		filesystems.Mounts = nil
	}
	var reportMS int64
	if err := tx.QueryRowContext(ctx, a.db.bind("SELECT report_interval_ms FROM hosts WHERE host_id=?"), host).Scan(&reportMS); err != nil {
		return err
	}
	maxGap := max(15*time.Second, 3*time.Duration(reportMS)*time.Millisecond)
	for _, rule := range a.cfg.Rules {
		if !rule.Enabled || (rule.Host != "" && rule.Host != host) {
			continue
		}
		values := make(map[string]*float64)
		authoritative := false
		switch rule.Metric {
		case "cpu":
			values[""] = nil
			for _, w := range sample.Windows {
				if rule.WindowMS != 0 && w.RequestedMS != float64(rule.WindowMS) {
					continue
				}
				if value, ok := w.CPU["cpu"]["busy"]; ok && w.Status == "ok" {
					values[""] = &value
				}
				break
			}
		case "memory":
			values[""] = utilization(sample.Memory, "used", "total")
		case "disk":
			authoritative = filesystems.Mounts != nil && filesystems.Issue == ""
			for _, mount := range filesystems.Mounts {
				if !mount.ReadOnly && (rule.Target == "" || rule.Target == mount.Target) {
					values[mount.Target] = utilization(mount.Capacity, "used_bytes", "total_bytes")
				}
			}
		case "service_stopped":
			authoritative = (sample.Processes.Services != nil || (sample.Processes.Enabled != nil && !*sample.Processes.Enabled)) && len(sample.Processes.Issues) == 0
			for _, svc := range sample.Processes.Services {
				if rule.Target != "" && rule.Target != svc.Name && rule.Target != svc.Key {
					continue
				}
				switch svc.State {
				case "stopped":
					v := 0.0
					values[svc.Key] = &v
				case "running":
					v := 1.0
					values[svc.Key] = &v
				default:
					values[svc.Key] = nil
				}
			}
		default:
			continue
		}
		rows, err := tx.QueryContext(ctx, a.db.bind("SELECT id,target FROM alert_instances WHERE host_id=? AND rule_id=? AND active_key IS NOT NULL"), host, rule.ID)
		if err != nil {
			return err
		}
		var removed []string
		for rows.Next() {
			var id, target string
			if err := rows.Scan(&id, &target); err != nil {
				rows.Close()
				return err
			}
			if _, ok := values[target]; !ok {
				if authoritative {
					removed = append(removed, id)
				} else {
					values[target] = nil
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range removed {
			if _, err := a.closeInstance(ctx, tx, id, "target_removed", now); err != nil {
				return err
			}
		}
		for target, value := range values {
			bad := value != nil && *value >= rule.Threshold
			if rule.Metric == "service_stopped" {
				bad = value != nil && *value == 0
			}
			if err := a.observe(ctx, tx, rule, host, target, value, bad, now.UnixNano(), now, maxGap); err != nil {
				return err
			}
		}
	}
	return nil
}

func alertRuleVersion(rule alertRule) string {
	return alertKey(strconv.FormatFloat(rule.Threshold, 'g', -1, 64), strconv.Itoa(rule.Trigger),
		strconv.Itoa(rule.Recover), strconv.Itoa(rule.WindowMS), rule.Host, rule.Target)
}
