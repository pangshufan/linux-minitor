package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureProcess(t *testing.T, root string, pid, parent int, start, ticks uint64, exe string, args ...string) {
	t.Helper()
	path := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(filepath.Join(path, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	stat := strings.Replace(processStat(filepath.Base(exe), start, ticks), " S 1 ", fmt.Sprintf(" S %d ", parent), 1)
	for name, data := range map[string]string{"stat": stat, "cmdline": strings.Join(append([]string{exe}, args...), "\x00") + "\x00", "status": "Uid: 1000\n", "io": "read_bytes: 4096\nwrite_bytes: 8192\n", "smaps_rollup": "Pss: 16 kB\n"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(filepath.Join(path, "exe"))
	if err := os.Symlink(exe, filepath.Join(path, "exe")); err != nil {
		t.Fatal(err)
	}
}
func collectFixture(c *processCollector, cfg config, at time.Time) (*metricSnapshot, rawSample) {
	up := 100.0
	boot := at.Add(-100 * time.Second)
	s := &metricSnapshot{CollectedAt: at, BootedAt: &boot, UptimeSeconds: &up, MemoryBytes: map[string]uint64{"total": 1 << 30}}
	raw := rawSample{at: at, counters: map[string]map[string]uint64{}}
	c.collect(cfg, s, &raw)
	return s, raw
}
func TestServiceConfigAndOptIn(t *testing.T) {
	for _, text := range []string{`[{"name":"x","match":{"executable":"relative"}}]`, `[{"name":"x","match":{"cgroup":"/"}}]`, `[{"name":"x","match":{"systemd_unit":"../x.service"}}]`, `[{"name":"x","match":{"executable":"/bin/x","cgroup":"/a"}}]`, `[{"name":"x","match":{"executable":"/x"},"typo":true}]`, `[{"name":"x","match":{"executable":"/x"}},{"name":"x","match":{"executable":"/y"}}]`, `null`} {
		if _, err := parseServiceConfig(text); err == nil {
			t.Fatalf("accepted invalid config: %s", text)
		}
	}
	if _, err := parseServiceConfig(`[{"name":"订单服务","match":{"executable":"/usr/bin/java","args_contains":["-jar","/opt/order.jar"]}}]`); err != nil {
		t.Fatal(err)
	}
	c := &processCollector{root: "/does-not-exist"}
	s, _ := collectFixture(c, config{processes: true, processLimit: 10}, time.Now())
	if len(s.Processes.Services) != 0 || len(s.Processes.Issues) > 0 || c.index != nil {
		t.Fatal("empty config scanned procfs")
	}
}
func TestNamedServicesDescendantsAndReparenting(t *testing.T) {
	root := t.TempDir()
	fixtureProcess(t, root, 42, 1, 300, 100, "/opt/app", "--role=orders")
	fixtureProcess(t, root, 43, 42, 301, 50, "/opt/worker")
	fixtureProcess(t, root, 44, 43, 302, 10, "/opt/helper")
	fixtureProcess(t, root, 45, 1, 303, 500, "/opt/unrelated")
	cfg := config{processes: true, processLimit: 20, processDiscovery: time.Second, processPSS: true, processPSSInterval: 30 * time.Second, services: []serviceConfig{
		{Name: "订单服务", Match: serviceMatch{Executable: "/opt/app", ArgsContains: []string{"--role=orders"}}},
		{Name: "工作进程", Match: serviceMatch{Executable: "/opt/worker"}},
	}}
	c := &processCollector{root: root, clock: 100}
	defer c.close()
	now := time.Now()
	s, _ := collectFixture(c, cfg, now)
	if len(s.Processes.Items) != 3 || s.Processes.Services[0].ProcessCount != 3 || s.Processes.Services[1].ProcessCount != 2 {
		t.Fatalf("membership: %+v", s.Processes)
	}
	for _, p := range s.Processes.Items {
		if p.PID == 44 && p.ParentKey != "process/43/301" {
			t.Fatalf("missing hierarchy: %+v", p)
		}
		if p.PID == 43 && len(p.ServiceKeys) != 2 {
			t.Fatal("overlap duplicated process")
		}
	}
	svc := s.Processes.Services[0]
	if svc.PSSSumBytes == nil || *svc.PSSSumBytes != 3*16*1024 {
		t.Fatalf("PSS: %+v", svc)
	}
	// A descendant survives its parent and is reparented to PID 1.
	if err := os.RemoveAll(filepath.Join(root, "42")); err != nil {
		t.Fatal(err)
	}
	fixtureProcess(t, root, 43, 1, 301, 70, "/opt/worker")
	s, _ = collectFixture(c, cfg, now.Add(2*time.Second))
	if s.Processes.Services[0].ProcessCount != 2 || s.Processes.Services[0].State != "degraded" || s.Processes.Services[0].Restarts != 0 {
		t.Fatalf("orphan lost: %+v", s.Processes.Services[0])
	}
	cfg.processLimit = 1
	s, raw := collectFixture(c, cfg, now.Add(4*time.Second))
	svc = s.Processes.Services[0]
	if !s.Processes.Truncated || svc.Complete || svc.RSSSumBytes != nil || raw.services[svc.Key].complete {
		t.Fatal("partial members produced complete totals")
	}
}
func TestServiceDiscoveryAndRestart(t *testing.T) {
	root := t.TempDir()
	fixtureProcess(t, root, 42, 1, 300, 100, "/opt/app")
	no := false
	cfg := config{processes: true, processLimit: 10, processDiscovery: 5 * time.Second, services: []serviceConfig{{Name: "app", Match: serviceMatch{Executable: "/opt/app"}, IncludeChildren: &no}}}
	c := &processCollector{root: root, clock: 100}
	defer c.close()
	now := time.Now()
	s, _ := collectFixture(c, cfg, now)
	if s.Processes.Services[0].Restarts != 0 {
		t.Fatal("initial observation counted as restart")
	}
	fixtureProcess(t, root, 43, 1, 400, 1, "/opt/app")
	s, _ = collectFixture(c, cfg, now.Add(time.Second))
	if s.Processes.Services[0].ProcessCount != 1 {
		t.Fatal("discovery interval ignored")
	}
	s, _ = collectFixture(c, cfg, now.Add(6*time.Second))
	if s.Processes.Services[0].ProcessCount != 2 {
		t.Fatal("new root not discovered")
	}
	// Remove both roots, observe stop, then reuse PID with a new start time.
	os.RemoveAll(filepath.Join(root, "42"))
	os.RemoveAll(filepath.Join(root, "43"))
	s, _ = collectFixture(c, cfg, now.Add(7*time.Second))
	if s.Processes.Services[0].State != "stopped" {
		t.Fatal("stop not detected")
	}
	fixtureProcess(t, root, 42, 1, 500, 1, "/opt/app")
	s, _ = collectFixture(c, cfg, now.Add(13*time.Second))
	if s.Processes.Services[0].Restarts != 1 || s.Processes.Services[0].ObservedExits != 2 {
		t.Fatalf("lifecycle: %+v", s.Processes.Services[0])
	}
}
func TestServiceRatesMembershipChange(t *testing.T) {
	now := time.Now()
	key := "service/app"
	proc := "process/42/100"
	base := rawSample{at: now, counters: map[string]map[string]uint64{proc: {"cpu_time_ns": 100, "read_bytes": 100}}, services: map[string]rawService{key: {members: []string{proc}, complete: true, sources: map[string]string{"cpu": "procfs_sum", "io": "procfs_sum"}}}}
	current := rawSample{at: now.Add(time.Second), counters: map[string]map[string]uint64{proc: {"cpu_time_ns": 100000100, "read_bytes": 110}}, services: base.services}
	w := calculateWindow(current, []rawSample{base}, time.Second, time.Second)
	if w.Rates[key]["cpu_percent"] != 10 || w.Rates[key]["read_bytes"] != 10 {
		t.Fatalf("rates: %+v", w)
	}
	current.services = map[string]rawService{key: {members: []string{proc, "process/43/101"}, complete: true, sources: map[string]string{"cpu": "procfs_sum", "io": "procfs_sum"}}}
	w = calculateWindow(current, []rawSample{base}, time.Second, time.Second)
	if _, ok := w.Rates[key]["cpu_percent"]; ok {
		t.Fatal("changing membership produced a proc sum")
	}
	current.services[key] = rawService{members: []string{proc, "process/43/101"}, complete: true, counterKey: "cgroup/1", sources: map[string]string{"cpu": "cgroup_v2", "io": "cgroup_v2"}}
	base.counters["cgroup/1"] = map[string]uint64{"cpu_time_ns": 0, "read_bytes": 100}
	current.counters["cgroup/1"] = map[string]uint64{"cpu_time_ns": 1000000000, "read_bytes": 200}
	w = calculateWindow(current, []rawSample{base}, time.Second, time.Second)
	if w.Rates[key]["cpu_percent"] != 100 || w.Rates[key]["read_bytes"] != 100 {
		t.Fatal("group counters should survive membership changes")
	}
}
func TestPSSSmapsFallback(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "smaps"), []byte("Rss: 100 kB\nPss: 12 kB\nPss: 4 kB\n"), 0600)
	if n, err := readProcessPSS(root); err != nil || n != 16384 {
		t.Fatalf("PSS=%d err=%v", n, err)
	}
}

func TestAsyncUnitLookupDoesNotBlockSampling(t *testing.T) {
	block := make(chan struct{})
	finished := make(chan struct{})
	c := &processCollector{root: t.TempDir(), clock: 100, asyncUnits: true, mounts: []cgroupMount{}, unitLookup: func(string) (string, error) { <-block; return "", nil }}
	cfg := config{processes: true, processLimit: 10, processDiscovery: 5 * time.Second, services: []serviceConfig{{Name: "app", Match: serviceMatch{SystemdUnit: "app.service"}}}}
	now := time.Now()
	go func() { collectFixture(c, cfg, now); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		close(block)
		t.Fatal("unit lookup blocked metrics collection")
	}
	close(block)
	tracker := c.services[serviceKey("app")]
	select {
	case result := <-tracker.unitUpdates:
		tracker.unitUpdates <- result
	case <-time.After(time.Second):
		t.Fatal("unit lookup did not finish")
	}
	s, _ := collectFixture(c, cfg, now.Add(time.Second))
	if s.Processes.Services[0].State != "stopped" {
		t.Fatal("async unit result not applied")
	}
}

func TestServiceMatchAnyGroupsMultipleExecutables(t *testing.T) {
	root := t.TempDir()
	fixtureProcess(t, root, 42, 1, 300, 100, "/jdk-a/bin/java", "first.jar")
	fixtureProcess(t, root, 43, 1, 301, 50, "/jdk-b/bin/java", "second.jar")
	fixtureProcess(t, root, 44, 1, 302, 10, "/jdk-b/bin/java", "other.jar")
	configs, err := parseServiceConfig(`[{"name":"app","match_any":[{"executable":"/jdk-a/bin/java","args_contains":["first.jar"]},{"executable":"/jdk-b/bin/java","args_contains":["second.jar"]}]}]`)
	if err != nil {
		t.Fatal(err)
	}
	collector := &processCollector{root: root, clock: 100}
	defer collector.close()
	snapshot, _ := collectFixture(collector, config{processes: true, processLimit: 10, services: configs}, time.Now())
	service := snapshot.Processes.Services[0]
	if service.State != "running" || service.ProcessCount != 2 || service.RootCount != 2 {
		t.Fatalf("unexpected service membership: %+v", service)
	}
}
