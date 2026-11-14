package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"monitor/internal/transport"
)

type alertRule struct {
	ID        string   `json:"id"`
	Metric    string   `json:"metric"`
	Enabled   bool     `json:"enabled"`
	Threshold float64  `json:"threshold"`
	Trigger   int      `json:"trigger_count"`
	Recover   int      `json:"recover_count"`
	WindowMS  int      `json:"window_ms,omitempty"`
	Severity  string   `json:"severity"`
	Host      string   `json:"host_id,omitempty"`
	Target    string   `json:"target,omitempty"`
	Receivers []string `json:"receivers"`
}

type alertReceiver struct {
	URL         string `json:"url"`
	TokenEnv    string `json:"token_env,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
	token       string
	timeout     time.Duration
}

type alertConfig struct {
	Enabled   bool                     `json:"enabled"`
	Rules     []alertRule              `json:"rules"`
	Receivers map[string]alertReceiver `json:"receivers"`
}

func defaultAlertRules() []alertRule {
	var rules []alertRule
	for _, d := range []struct {
		metric    string
		threshold float64
		count     int
		severity  string
	}{
		{"cpu", 90, 3, "warning"}, {"memory", 90, 3, "warning"}, {"disk", 90, 3, "warning"},
		{"service_stopped", 0, 2, "critical"}, {"host_offline", 60, 1, "critical"},
		{"collection_stalled", 60, 1, "warning"}, {"snapshot_missing", 90, 1, "warning"},
	} {
		rules = append(rules, alertRule{ID: d.metric, Metric: d.metric, Enabled: true, Threshold: d.threshold,
			Trigger: d.count, Recover: 2, Severity: d.severity})
	}
	return rules
}

func loadAlertConfig(raw string) (alertConfig, error) {
	cfg := alertConfig{Rules: defaultAlertRules()}
	var input struct {
		Enabled   bool                       `json:"enabled"`
		Rules     map[string]json.RawMessage `json:"rules"`
		Receivers map[string]alertReceiver   `json:"receivers"`
	}
	if raw == "[]" { // Default value of configfile.JSONValue.
		raw = "{}"
	}
	if err := decodeAlertJSON(raw, &input); err != nil {
		return cfg, fmt.Errorf("alerts: %w", err)
	}
	cfg.Enabled, cfg.Receivers = input.Enabled, input.Receivers
	templates := make(map[string]alertRule)
	ids := make([]string, 0, len(cfg.Rules)+len(input.Rules))
	for _, rule := range cfg.Rules {
		templates[rule.Metric] = rule
		ids = append(ids, rule.ID)
	}
	var custom []string
	for id := range input.Rules {
		if _, builtin := templates[id]; !builtin {
			custom = append(custom, id)
		}
	}
	slices.Sort(custom)
	ids = append(ids, custom...)
	if len(ids) > 128 {
		return cfg, fmt.Errorf("at most 128 alert rules are supported")
	}
	cfg.Rules = cfg.Rules[:0]
	for _, id := range ids {
		rule, builtin := templates[id]
		patch, overridden := input.Rules[id]
		if !builtin {
			var kind struct {
				Metric string `json:"metric"`
			}
			if err := json.Unmarshal(patch, &kind); err != nil {
				return cfg, fmt.Errorf("alert rule %s: %w", id, err)
			}
			var ok bool
			rule, ok = templates[kind.Metric]
			if !ok {
				return cfg, fmt.Errorf("alert rule %s requires a supported metric", id)
			}
			rule.ID = id
		}
		metric := rule.Metric
		if overridden {
			if err := decodeAlertJSON(string(patch), &rule); err != nil {
				return cfg, fmt.Errorf("alert rule %s: %w", id, err)
			}
		}
		if !transport.ValidHostID(id) || rule.ID != id || rule.Metric != metric ||
			rule.Trigger < 1 || rule.Trigger > 100 || rule.Recover < 1 || rule.Recover > 100 ||
			rule.WindowMS < 0 || (rule.Metric != "cpu" && rule.WindowMS != 0) ||
			(rule.Target != "" && rule.Metric != "disk" && rule.Metric != "service_stopped") ||
			(rule.Host != "" && !transport.ValidHostID(rule.Host)) ||
			(rule.Severity != "warning" && rule.Severity != "critical" && rule.Severity != "info") {
			return cfg, fmt.Errorf("invalid alert rule %s", id)
		}
		switch rule.Metric {
		case "cpu", "memory", "disk":
			if rule.Threshold <= 0 || rule.Threshold > 100 {
				return cfg, fmt.Errorf("alert rule %s threshold must be in (0, 100]", rule.ID)
			}
		case "service_stopped":
			if rule.Threshold != 0 {
				return cfg, fmt.Errorf("alert rule %s requires threshold 0", rule.ID)
			}
		default:
			if rule.Threshold < 1 || rule.Threshold > 86400 {
				return cfg, fmt.Errorf("alert rule %s threshold must be 1–86400 seconds", rule.ID)
			}
		}
		for _, name := range rule.Receivers {
			if _, ok := cfg.Receivers[name]; !ok {
				return cfg, fmt.Errorf("alert rule %s references unknown receiver %q", rule.ID, name)
			}
		}
		cfg.Rules = append(cfg.Rules, rule)
	}
	for name, receiver := range cfg.Receivers {
		u, err := url.Parse(receiver.URL)
		if !transport.ValidHostID(name) || err != nil || u.Hostname() == "" ||
			(u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
			return cfg, fmt.Errorf("invalid alert receiver %q", name)
		}
		receiver.timeout = 5 * time.Second
		if receiver.Timeout != "" {
			receiver.timeout, err = time.ParseDuration(receiver.Timeout)
			if err != nil || receiver.timeout <= 0 || receiver.timeout > time.Minute {
				return cfg, fmt.Errorf("invalid timeout for alert receiver %q", name)
			}
		}
		if receiver.MaxAttempts == 0 {
			receiver.MaxAttempts = 5
		}
		if receiver.MaxAttempts < 1 || receiver.MaxAttempts > 20 {
			return cfg, fmt.Errorf("invalid max_attempts for alert receiver %q", name)
		}
		if receiver.TokenEnv != "" && cfg.Enabled {
			receiver.token = os.Getenv(receiver.TokenEnv)
			if strings.TrimSpace(receiver.token) == "" || strings.ContainsAny(receiver.token, "\r\n") {
				return cfg, fmt.Errorf("invalid token environment variable for alert receiver %q", name)
			}
		}
		cfg.Receivers[name] = receiver
	}
	return cfg, nil
}

func decodeAlertJSON(raw string, target any) error {
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return fmt.Errorf("expected JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
