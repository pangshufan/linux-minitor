//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type serviceTracker struct {
	serviceLifecycle
	discovered  time.Time
	members     map[int]string
	roots       map[int]string
	group       string
	resolveErr  error
	unitPending bool
	unitUpdates chan unitResolution
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
				if target.Match.SystemdUnit != "" && pids == nil && os.IsNotExist(err) {
					// A cached systemd cgroup vanished. Treat its membership as empty
					// and refresh the unit mapping on the next collection.
					t.discovered = time.Time{}
					svc.Cgroup = ""
				} else if err != nil {
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
		updateServiceLifecycle(&t.serviceLifecycle, &svc)
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
	if fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed" {
		return "", nil
	}
	group := fields["ControlGroup"]
	if group == "" {
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
