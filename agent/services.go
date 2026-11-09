package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	SystemdUnit  string   `json:"systemd_unit,omitempty"`
	Cgroup       string   `json:"cgroup,omitempty"`
	Executable   string   `json:"executable,omitempty"`
	ArgsContains []string `json:"args_contains,omitempty"`
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
			if m.SystemdUnit != "" || m.Cgroup != "" || m.Executable != "" || len(m.ArgsContains) > 0 {
				return nil, fmt.Errorf("service %q cannot combine match and match_any", s.Name)
			}
			if len(s.MatchAny) == 0 || len(s.MatchAny) > 32 {
				return nil, fmt.Errorf("service %q needs 1–32 match_any selectors", s.Name)
			}
			for _, selector := range s.MatchAny {
				if selector.SystemdUnit != "" || selector.Cgroup != "" || !filepath.IsAbs(selector.Executable) || !validServiceArgs(selector.ArgsContains) {
					return nil, fmt.Errorf("service %q match_any requires absolute executable paths and valid args_contains", s.Name)
				}
			}
			continue
		}
		n := 0
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
			return nil, fmt.Errorf("service %q needs exactly one selector: systemd_unit, cgroup or executable with optional args_contains", s.Name)
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
type serviceTracker struct {
	lastToken               string
	lastActive              bool
	discovered              time.Time
	members                 map[int]string
	roots                   map[int]string
	lastMembers             map[string]bool
	lastRoots               map[string]bool
	seen                    bool
	starts, exits, restarts uint64
	group                   string
	resolveErr              error
	unitPending             bool
	unitUpdates             chan unitResolution
}
type rawService struct {
	members    []string
	complete   bool
	counterKey string
	sources    map[string]string
}
type pssCache struct {
	at    time.Time
	value uint64
	err   error
}

func (c *processCollector) stat(pid int) (processMetric, error) {
	data, err := os.ReadFile(filepath.Join(c.root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return processMetric{}, err
	}
	return parseProcessStat(pid, string(data), uint64(os.Getpagesize()))
}
func (c *processCollector) discover(cfg serviceConfig, t *serviceTracker, now time.Time) error {
	index, discoveryErr := c.processIndex(now)
	if index == nil {
		return discoveryErr
	}
	roots := map[int]string{}
	selectors := cfg.MatchAny
	if len(selectors) == 0 {
		selectors = []serviceMatch{cfg.Match}
	}
	byExecutable := make(map[string][]serviceMatch)
	for _, selector := range selectors {
		executable := selector.Executable
		if executable != "" {
			if resolved, err := filepath.EvalSymlinks(executable); err == nil {
				executable = resolved
			}
		}
		byExecutable[executable] = append(byExecutable[executable], selector)
	}
	unreadable := 0
	for pid, p := range index {
		path := filepath.Join(c.root, strconv.Itoa(pid))
		matches := byExecutable[""]
		if len(matches) == 0 {
			exe, err := os.Readlink(filepath.Join(path, "exe"))
			if err != nil {
				if !os.IsNotExist(err) {
					unreadable++
				}
				continue
			}
			matches = byExecutable[strings.TrimSuffix(exe, " (deleted)")]
		}
		var command []byte
		for _, m := range matches {
			if len(m.legacyNames) > 0 && !m.legacyNames[p.Name] {
				continue
			}
			if len(m.ArgsContains) > 0 || m.legacyPattern != "" {
				if command == nil {
					var err error
					command, err = readProcessCommand(filepath.Join(path, "cmdline"))
					if err != nil {
						if !os.IsNotExist(err) {
							unreadable++
						}
						break
					}
				}
				if m.legacyPattern != "" && !strings.Contains(strings.ReplaceAll(string(command), "\x00", " "), m.legacyPattern) {
					continue
				}
				args := strings.Split(string(command), "\x00")
				if !slicesContainsAll(args[1:], m.ArgsContains) {
					continue
				}
			}
			roots[pid] = p.Key
			break
		}
	}

	members := map[int]string{}
	for pid, key := range roots {
		members[pid] = key
	}
	if cfg.children() {
		// Preserve known descendants after reparenting, while validating start ticks.
		for pid, key := range t.members {
			if p, ok := index[pid]; ok && p.Key == key && t.roots[pid] != key {
				members[pid] = key
			}
		}
		children := map[int][]int{}
		for pid, p := range index {
			children[int(p.PPID)] = append(children[int(p.PPID)], pid)
		}
		queue := []int{}
		for pid := range members {
			queue = append(queue, pid)
		}
		for i := 0; i < len(queue); i++ {
			for _, pid := range children[queue[i]] {
				if _, ok := members[pid]; !ok {
					members[pid] = index[pid].Key
					queue = append(queue, pid)
				}
			}
		}
	}
	t.members, t.roots, t.discovered = members, roots, now
	if discoveryErr != nil {
		return discoveryErr
	}
	if unreadable > 0 {
		return fmt.Errorf("%d processes unreadable during discovery", unreadable)
	}
	return nil
}

func (c *processCollector) collect(cfg config, s *metricSnapshot, raw *rawSample) {
	out := &processSnapshot{Enabled: cfg.processes, ClockTicks: c.clock, Items: []processMetric{}, Services: []serviceSnapshot{}, LifecycleSource: "procfs"}
	s.Processes = out
	c.groupMemberships = map[string]groupMembership{}
	c.index = nil // Share the discovery index only within this collection pass.
	configs := configuredServices(cfg)
	if !cfg.processes || len(configs) == 0 {
		return
	}
	if c.services == nil {
		c.services = map[string]*serviceTracker{}
	}
	if c.unitLookup == nil {
		c.unitLookup = lookupSystemdCgroup
		c.asyncUnits = true
	}
	if c.mounts == nil {
		c.mounts, _ = readCgroupMounts(filepath.Join(c.root, "self", "mountinfo"))
	}
	if c.clock == 0 {
		out.Issues = map[string]string{"clock_ticks": "AT_CLKTCK unavailable; CPU and start time omitted"}
	}
	exited := c.pollProcessExits()
	all := map[string]processMetric{}
	raw.services = map[string]rawService{}

	for _, target := range configs {
		key := serviceKey(target.Name)
		t := c.services[key]
		if t == nil {
			t = &serviceTracker{}
			c.services[key] = t
		}
		svc := serviceSnapshot{Key: key, Name: target.Name, State: "stopped", ProcessKeys: []string{}, RootKeys: []string{}, Complete: true, Sources: map[string]string{}, Issues: map[string]string{}}
		interval := cfg.processDiscovery
		if interval <= 0 {
			interval = 5 * time.Second
		}
		lookupDue := t.discovered.IsZero() || s.CollectedAt.Sub(t.discovered) >= interval
		discover := lookupDue || exited
		if target.Match.SystemdUnit != "" || target.Match.Cgroup != "" {
			svc.MembershipSource = "cgroup"
			if target.Match.SystemdUnit != "" {
				if c.asyncUnits {
					if t.unitUpdates == nil {
						t.unitUpdates = make(chan unitResolution, 1)
					}
					select {
					case result := <-t.unitUpdates:
						t.group, t.resolveErr = result.group, result.err
						t.unitPending = false
					default:
					}
					if lookupDue && !t.unitPending {
						t.unitPending = true
						t.discovered = s.CollectedAt
						if t.group == "" && t.resolveErr == nil {
							t.resolveErr = errors.New("systemd lookup pending")
						}
						updates, unit, lookup := t.unitUpdates, target.Match.SystemdUnit, c.unitLookup
						go func() { group, err := lookup(unit); updates <- unitResolution{group, err} }()
					}
				} else if lookupDue {
					t.group, t.resolveErr = c.unitLookup(target.Match.SystemdUnit)
					t.discovered = s.CollectedAt
				}
			} else if discover {
				t.group = target.Match.Cgroup
				t.resolveErr = nil
				t.discovered = s.CollectedAt
			}

			svc.Cgroup = t.group
			t.members = map[int]string{}
			if t.resolveErr != nil {
				svc.Issues["systemd"] = t.resolveErr.Error()
				svc.Complete = false
			} else if t.group != "" {
				pids, err := c.groupPIDs(t.group, target.children())
				if err != nil {
					svc.Issues["membership"] = err.Error()
					svc.Complete = false
				}
				t.members = map[int]string{}
				for _, pid := range pids {
					p, err := c.stat(pid)
					if err == nil {
						t.members[pid] = p.Key
					} else {
						svc.Complete = false
						svc.Issues["process_stat"] = "a cgroup member disappeared or could not be read"
					}
				}
			}
		} else {
			svc.MembershipSource = "process_tree"
			if !discover {
				for pid, expected := range t.roots {
					p, err := c.stat(pid)
					if err != nil || p.Key != expected {
						discover = true
						break
					}
				}
			}
			if discover {
				t.resolveErr = c.discover(target, t, s.CollectedAt)
			}
			if t.resolveErr != nil {
				svc.Issues["discovery"] = t.resolveErr.Error()
				svc.Complete = false
			}
		}
		live := map[int]processMetric{}
		for pid, expected := range t.members {
			p, err := c.stat(pid)
			if err != nil || p.Key != expected {
				if err != nil && !os.IsNotExist(err) {
					svc.Complete = false
					svc.Issues["process_stat"] = err.Error()
				}
				continue
			}
			if p.State == "Z" || p.State == "X" || p.State == "x" {
				continue
			}
			live[pid] = p
			svc.ProcessKeys = append(svc.ProcessKeys, p.Key)
		}
		for _, p := range live {
			parent, ok := live[int(p.PPID)]
			if ok && parent.StartTicks <= p.StartTicks && parent.Key != p.Key {
				p.ParentKey = parent.Key
			} else {
				svc.RootKeys = append(svc.RootKeys, p.Key)
			}
			if existing, ok := all[p.Key]; ok {
				p = existing
			}
			p.ServiceKeys = append(p.ServiceKeys, key)
			if target.Match.legacyPattern != "" || len(target.Match.legacyNames) > 0 {
				p.MatchedPatterns = append(p.MatchedPatterns, target.Name)
			}
			all[p.Key] = p
		}
		sort.Strings(svc.ProcessKeys)
		sort.Strings(svc.RootKeys)
		svc.ProcessCount, svc.RootCount = len(svc.ProcessKeys), len(svc.RootKeys)
		if svc.MembershipSource == "cgroup" {
			svc.groupToken = c.groupToken(svc.Cgroup)
		} else {
			for pid, expected := range t.roots {
				if p, ok := live[pid]; ok && p.Key == expected {
					svc.lifecycleRoots = append(svc.lifecycleRoots, expected)
				}
			}
		}
		updateServiceLifecycle(t, &svc)
		if svc.ProcessCount > 0 {
			svc.State = "running"
		}
		if !svc.Complete {
			if svc.ProcessCount == 0 {
				svc.State = "unknown"
			} else {
				svc.State = "degraded"
			}
		}
		if len(target.MatchAny) > 0 || target.Match.Executable != "" || target.Match.legacyPattern != "" || len(target.Match.legacyNames) > 0 {
			rootAlive := false
			for pid, expected := range t.roots {
				if p, ok := live[pid]; ok && p.Key == expected {
					rootAlive = true
					break
				}
			}
			if len(live) > 0 && !rootAlive {
				svc.State = "degraded"
				svc.Issues["root"] = "configured root process is absent; known descendants remain"
			}
		}
		counterKey := ""
		if svc.Cgroup != "" && t.resolveErr == nil && target.children() {
			counters, memory, sources, identity, issues := c.groupMetrics(svc.Cgroup)
			svc.CgroupMemoryBytes = memory
			for name, source := range sources {
				svc.Sources[name] = source
			}
			for name, issue := range issues {
				svc.Issues[name] = issue
			}
			if len(counters) > 0 {
				counterKey = "cgroup/" + identity
				raw.counters[counterKey] = counters
			}
		}
		if svc.Sources["cpu"] == "" {
			svc.Sources["cpu"] = "procfs_sum"
		}
		if svc.Sources["io"] == "" {
			svc.Sources["io"] = "procfs_sum"
		}
		svc.Sources["rss_sum"] = "procfs"
		if cfg.processPSS {
			svc.Sources["pss_sum"] = "procfs_pss"
		}
		raw.services[key] = rawService{members: svc.ProcessKeys, complete: svc.Complete, counterKey: counterKey, sources: svc.Sources}
		out.Services = append(out.Services, svc)
	}
	byPID := map[int]processMetric{}
	for _, p := range all {
		byPID[p.PID] = p
	}
	candidates := []processMetric{}
	for _, p := range all {
		p.ParentKey = ""
		if parent, ok := byPID[int(p.PPID)]; ok && parent.Key != p.Key && parent.StartTicks <= p.StartTicks {
			p.ParentKey = parent.Key
		}
		candidates = append(candidates, p)
	}
	c.enrich(cfg, s, raw, out, candidates)
	reported := map[string]processMetric{}
	for _, p := range out.Items {
		reported[p.Key] = p
	}
	out.Matches = map[string]int{}
	for i := range out.Services {
		svc := &out.Services[i]
		out.Matches[svc.Name] = svc.ProcessCount
		var rss, threads, pss uint64
		fds := 0
		fdOK, pssOK := true, true
		var oldest *time.Time
		for _, key := range svc.ProcessKeys {
			p, ok := reported[key]
			if !ok {
				continue
			}
			svc.ReportedCount++
			rss += p.RSSBytes
			threads += p.Threads
			if p.FDCount == nil {
				fdOK = false
			} else {
				fds += *p.FDCount
			}
			if p.PSSBytes == nil {
				pssOK = false
			} else {
				pss += *p.PSSBytes
				if oldest == nil || p.PSSAt.Before(*oldest) {
					oldest = p.PSSAt
				}
			}
		}
		if svc.ReportedCount != svc.ProcessCount {
			svc.Complete = false
			svc.Issues["limit_or_race"] = "some service members were not reported"
			if svc.ProcessCount > 0 {
				svc.State = "degraded"
			}
		}
		if svc.Complete {
			svc.RSSSumBytes = &rss
			svc.Threads = &threads
			if fdOK {
				svc.FDSum = &fds
			}
			if cfg.processPSS && pssOK {
				svc.PSSSumBytes = &pss
				svc.PSSOldestAt = oldest
			}
		}
		rs := raw.services[svc.Key]
		rs.complete = svc.Complete
		raw.services[svc.Key] = rs

	}
	c.trackProcessHandles(out.Items)
	if !c.pidfdUnavailable && len(c.pidfds) > 0 {
		out.LifecycleSource = "pidfd+procfs"
	}
	for key := range c.pss {
		if _, ok := reported[key]; !ok {
			delete(c.pss, key)
		}
	}
}

func updateServiceLifecycle(t *serviceTracker, svc *serviceSnapshot) {
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
func lookupSystemdCgroup(unit string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := commandOutput(ctx, "systemctl", "show", "--property=ControlGroup", "--property=LoadState", "--property=ActiveState", "--", unit)
	if err != nil {
		return "", fmt.Errorf("resolve unit %s: %w", unit, err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = v
		}
	}
	if fields["LoadState"] == "not-found" {
		return "", fmt.Errorf("systemd unit %s not found", unit)
	}
	group := fields["ControlGroup"]
	if group == "" {
		if fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed" {
			return "", nil
		}
		return "", errors.New("unit cgroup unavailable")
	}
	if !filepath.IsAbs(group) || group == "/" || filepath.Clean(group) != group {
		return "", errors.New("invalid unit cgroup")
	}
	return group, nil
}
func readProcessPSS(path string) (uint64, error) {
	for _, name := range []string{"smaps_rollup", "smaps"} {
		var sum uint64
		found := false
		var parseErr error
		err := readFields(filepath.Join(path, name), func(f []string) {
			if len(f) == 3 && f[0] == "Pss:" && f[2] == "kB" {
				n, err := strconv.ParseUint(f[1], 10, 64)
				if err != nil {
					parseErr = err
					return
				}
				sum += n * 1024
				found = true
			}
		})
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		if parseErr != nil {
			return 0, parseErr
		}
		if found {
			return sum, nil
		}
		return 0, errors.New("Pss unavailable")
	}
	return 0, os.ErrNotExist
}

func (c *processCollector) processIndex(now time.Time) (map[int]processMetric, error) {
	if c.index != nil && c.indexAt.Equal(now) {
		return c.index, c.indexErr
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, err
	}
	index := map[int]processMetric{}
	unreadable := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := c.stat(pid)
		if err != nil {
			if !os.IsNotExist(err) {
				unreadable++
			}
			continue
		}
		index[pid] = p
	}
	c.index, c.indexAt, c.indexErr = index, now, nil
	if unreadable > 0 {
		c.indexErr = fmt.Errorf("%d processes unreadable during discovery", unreadable)
	}
	return index, c.indexErr
}

type unitResolution struct {
	group string
	err   error
}
