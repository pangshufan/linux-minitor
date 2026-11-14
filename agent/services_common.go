package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type serviceConfig struct {
	Name            string         `json:"name"`
	Match           serviceMatch   `json:"match"`
	MatchAny        []serviceMatch `json:"match_any,omitempty"`
	IncludeChildren *bool          `json:"include_children,omitempty"`
}
type serviceMatch struct {
	WindowsService string   `json:"windows_service,omitempty"`
	SystemdUnit    string   `json:"systemd_unit,omitempty"`
	Cgroup         string   `json:"cgroup,omitempty"`
	Executable     string   `json:"executable,omitempty"`
	ArgsContains   []string `json:"args_contains,omitempty"`
	// Read-only compatibility selectors, not accepted as public JSON fields.
	legacyPattern string
	legacyNames   map[string]bool
}

func (s serviceConfig) children() bool { return s.IncludeChildren == nil || *s.IncludeChildren }
func serviceKey(name string) string    { return "service/" + url.PathEscape(name) }
func parseServiceConfig(text string) ([]serviceConfig, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var configs []serviceConfig
	if err := dec.Decode(&configs); err != nil {
		return nil, fmt.Errorf("process-services: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("unexpected content after process-services")
	}
	if strings.TrimSpace(text) == "null" {
		return nil, errors.New("process-services must be an array")
	}
	if len(configs) > 128 {
		return nil, errors.New("at most 128 services are supported")
	}
	names := map[string]bool{}
	for _, s := range configs {
		if strings.TrimSpace(s.Name) != s.Name || s.Name == "" || len(s.Name) > 128 || !utf8.ValidString(s.Name) || strings.ContainsAny(s.Name, "\x00\r\n\t") {
			return nil, errors.New("service name must be a nonempty unique UTF-8 name of at most 128 bytes")
		}
		if names[s.Name] {
			return nil, fmt.Errorf("duplicate service name %q", s.Name)
		}
		names[s.Name] = true
		m := s.Match
		if s.MatchAny != nil {
			if m.WindowsService != "" || m.SystemdUnit != "" || m.Cgroup != "" || m.Executable != "" || len(m.ArgsContains) > 0 {
				return nil, fmt.Errorf("service %q cannot combine match and match_any", s.Name)
			}
			if len(s.MatchAny) == 0 || len(s.MatchAny) > 32 {
				return nil, fmt.Errorf("service %q needs 1–32 match_any selectors", s.Name)
			}
			for _, selector := range s.MatchAny {
				if selector.WindowsService != "" || selector.SystemdUnit != "" || selector.Cgroup != "" || !filepath.IsAbs(selector.Executable) || !validServiceArgs(selector.ArgsContains) {
					return nil, fmt.Errorf("service %q match_any requires absolute executable paths and valid args_contains", s.Name)
				}
			}
			continue
		}
		n := 0
		if m.WindowsService != "" {
			n++
			if strings.TrimSpace(m.WindowsService) != m.WindowsService || len(m.WindowsService) > 256 || strings.ContainsAny(m.WindowsService, "/\\\x00\r\n") {
				return nil, fmt.Errorf("invalid Windows service name for %q", s.Name)
			}
		}
		if m.SystemdUnit != "" {
			n++
			if strings.ContainsAny(m.SystemdUnit, "/\x00\r\n\t ") || strings.HasPrefix(m.SystemdUnit, "-") || !strings.HasSuffix(m.SystemdUnit, ".service") {
				return nil, fmt.Errorf("invalid systemd unit for %q", s.Name)
			}
		}
		if m.Cgroup != "" {
			n++
			if !filepath.IsAbs(m.Cgroup) || filepath.Clean(m.Cgroup) != m.Cgroup || m.Cgroup == "/" {
				return nil, fmt.Errorf("cgroup for %q must be a non-root absolute hierarchy path", s.Name)
			}
		}
		if m.Executable != "" {
			n++
			if !filepath.IsAbs(m.Executable) {
				return nil, fmt.Errorf("executable for %q must be absolute", s.Name)
			}
		}
		if n != 1 || (m.Executable == "" && len(m.ArgsContains) > 0) {
			return nil, fmt.Errorf("service %q needs exactly one selector: systemd_unit, cgroup, windows_service or executable", s.Name)
		}
		if !validServiceArgs(m.ArgsContains) {
			return nil, fmt.Errorf("invalid args_contains for %q", s.Name)
		}
	}
	return configs, nil
}

func validServiceArgs(args []string) bool {
	for _, arg := range args {
		if arg == "" || strings.ContainsRune(arg, 0) {
			return false
		}
	}
	return true
}

func slicesContainsAll(args, required []string) bool {
	for _, arg := range required {
		if !slices.Contains(args, arg) {
			return false
		}
	}
	return true
}

func configuredServices(cfg config) []serviceConfig {
	if len(cfg.services) > 0 {
		return cfg.services
	}
	// Older explicit filters become named services; an empty selection never means all processes.
	names := map[string]bool{}
	for _, name := range strings.Split(cfg.processNames, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names[name] = true
		}
	}
	out := []serviceConfig{}
	for _, pattern := range strings.Split(cfg.processMatch, ",") {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			out = append(out, serviceConfig{Name: pattern, Match: serviceMatch{legacyPattern: pattern, legacyNames: names}})
		}
	}
	if len(out) == 0 {
		keys := []string{}
		for name := range names {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			out = append(out, serviceConfig{Name: name, Match: serviceMatch{legacyNames: map[string]bool{name: true}}})
		}
	}
	return out
}

type serviceSnapshot struct {
	lifecycleRoots    []string
	groupToken        string
	Key               string            `json:"key"`
	Name              string            `json:"name"`
	State             string            `json:"state"`
	MembershipSource  string            `json:"membership_source"`
	Cgroup            string            `json:"cgroup,omitempty"`
	ProcessKeys       []string          `json:"process_keys"`
	RootKeys          []string          `json:"root_keys"`
	ProcessCount      int               `json:"process_count"`
	ReportedCount     int               `json:"reported_count"`
	RootCount         int               `json:"root_count"`
	Complete          bool              `json:"complete"`
	Threads           *uint64           `json:"threads,omitempty"`
	RSSSumBytes       *uint64           `json:"rss_sum_bytes,omitempty"`
	FDSum             *int              `json:"fd_sum,omitempty"`
	PSSSumBytes       *uint64           `json:"pss_sum_bytes,omitempty"`
	PSSOldestAt       *time.Time        `json:"pss_oldest_at,omitempty"`
	CgroupMemoryBytes *uint64           `json:"cgroup_memory_bytes,omitempty"`
	Sources           map[string]string `json:"metric_sources"`
	ObservedStarts    uint64            `json:"observed_starts"`
	ObservedExits     uint64            `json:"observed_exits"`
	Restarts          uint64            `json:"observed_restarts"`
	Issues            map[string]string `json:"issues,omitempty"`
}
type serviceLifecycle struct {
	lastToken               string
	lastActive              bool
	lastMembers             map[string]bool
	lastRoots               map[string]bool
	seen                    bool
	starts, exits, restarts uint64
}

type rawService struct {
	members    []string
	complete   bool
	counterKey string
	sources    map[string]string
}

func updateServiceLifecycle(t *serviceLifecycle, svc *serviceSnapshot) {
	if !svc.Complete {
		svc.ObservedStarts, svc.ObservedExits, svc.Restarts = t.starts, t.exits, t.restarts
		return
	}
	members := map[string]bool{}
	for _, key := range svc.ProcessKeys {
		members[key] = true
		if t.seen && !t.lastMembers[key] {
			t.starts++
		}
	}
	for key := range t.lastMembers {
		if !members[key] && svc.Complete {
			t.exits++
		}
	}
	roots := map[string]bool{}
	overlap := false
	for _, key := range svc.lifecycleRoots {
		roots[key] = true
		if t.lastRoots[key] {
			overlap = true
		}
	}
	if svc.MembershipSource == "cgroup" {
		if svc.ProcessCount > 0 && t.seen && (!t.lastActive || (svc.groupToken != "" && t.lastToken != "" && svc.groupToken != t.lastToken)) {
			t.restarts++
		}
	} else if len(roots) > 0 && t.seen && !overlap {
		t.restarts++
	}
	if svc.Complete {
		t.lastToken = svc.groupToken
		t.lastActive = svc.ProcessCount > 0
		t.lastMembers = members
		t.lastRoots = roots
		if len(members) > 0 {
			t.seen = true
		}
	}
	svc.ObservedStarts, svc.ObservedExits, svc.Restarts = t.starts, t.exits, t.restarts
}
