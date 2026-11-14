//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func groupFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestCgroupV2MembershipAndMetrics(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	mount := filepath.Join(root, "cgroup")
	group := filepath.Join(mount, "system.slice", "orders.service")
	fixtureProcess(t, proc, 42, 1, 300, 10, "/opt/app")
	fixtureProcess(t, proc, 43, 1, 301, 20, "/opt/worker")
	fixtureProcess(t, proc, 44, 43, 302, 5, "/opt/helper")
	for name, text := range map[string]string{"cgroup.procs": "42\n43\n", "cpu.stat": "usage_usec 1000000\n", "memory.current": "65536\n", "io.stat": "8:0 rbytes=100 wbytes=200\n8:16 rbytes=50 wbytes=70\n"} {
		groupFile(t, group, name, text)
	}
	groupFile(t, filepath.Join(group, "workers"), "cgroup.procs", "44\n")
	c := &processCollector{root: proc, clock: 100, mounts: []cgroupMount{{path: mount, root: "/", v2: true}}}
	defer c.close()
	cfg := config{processes: true, processLimit: 10, services: []serviceConfig{{Name: "订单", Match: serviceMatch{Cgroup: "/system.slice/orders.service"}}}}
	now := time.Now()
	s, base := collectFixture(c, cfg, now)
	svc := s.Processes.Services[0]
	if svc.ProcessCount != 3 || svc.RootCount != 2 || svc.CgroupMemoryBytes == nil || *svc.CgroupMemoryBytes != 65536 || svc.Sources["cpu"] != "cgroup_v2" {
		t.Fatalf("group: %+v", svc)
	}
	groupFile(t, group, "cpu.stat", "usage_usec 2000000\n")
	groupFile(t, group, "io.stat", "8:0 rbytes=250 wbytes=370\n")
	_, current := collectFixture(c, cfg, now.Add(time.Second))
	w := calculateWindow(current, []rawSample{base}, time.Second, time.Second)
	if w.Rates[svc.Key]["cpu_percent"] != 100 || w.Rates[svc.Key]["read_bytes"] != 100 || w.Rates[svc.Key]["write_bytes"] != 100 {
		t.Fatalf("group rate: %+v", w)
	}
	no := false
	cfg.services[0].IncludeChildren = &no
	s, _ = collectFixture(c, cfg, now.Add(2*time.Second))
	if s.Processes.Services[0].ProcessCount != 2 || s.Processes.Services[0].Sources["cpu"] != "procfs_sum" {
		t.Fatal("direct-members selector included subgroup totals")
	}
}
func TestCgroupV1AndMountRoot(t *testing.T) {
	root := t.TempDir()
	cpu := filepath.Join(root, "cpu")
	mem := filepath.Join(root, "memory")
	io := filepath.Join(root, "io")
	groupFile(t, filepath.Join(cpu, "app"), "cpuacct.usage", "1200000000\n")
	groupFile(t, filepath.Join(mem, "app"), "memory.usage_in_bytes", "16384\n")
	groupFile(t, filepath.Join(io, "app"), "blkio.throttle.io_service_bytes_recursive", "8:0 Read 100\n8:0 Write 200\n8:0 Total 300\nTotal 300\n")
	c := &processCollector{mounts: []cgroupMount{{path: cpu, root: "/slice", controllers: map[string]bool{"cpuacct": true}}, {path: mem, root: "/slice", controllers: map[string]bool{"memory": true}}, {path: io, root: "/slice", controllers: map[string]bool{"blkio": true}}}}
	counters, memory, sources, _, _ := c.groupMetrics("/slice/app")
	if counters["cpu_time_ns"] != 1200000000 || counters["read_bytes"] != 100 || counters["write_bytes"] != 200 || memory == nil || *memory != 16384 || sources["cpu"] != "cgroup_v1" {
		t.Fatal("v1 metrics not parsed")
	}
	if _, ok := c.mounts[0].directory("/other/app"); ok {
		t.Fatal("escaped mount root")
	}
}
func TestSystemdLookupAndUnknownMembership(t *testing.T) {
	root := t.TempDir()
	c := &processCollector{root: root, clock: 100, mounts: []cgroupMount{}, unitLookup: func(string) (string, error) { return "", nil }}
	cfg := config{processes: true, processLimit: 10, services: []serviceConfig{{Name: "app", Match: serviceMatch{SystemdUnit: "app.service"}}}}
	s, _ := collectFixture(c, cfg, time.Now())
	if s.Processes.Services[0].State != "stopped" {
		t.Fatal("inactive unit should be stopped")
	}
	c.unitLookup = func(string) (string, error) { return "", os.ErrPermission }
	c.services = nil
	s, _ = collectFixture(c, cfg, time.Now())
	if s.Processes.Services[0].State != "unknown" || s.Processes.Services[0].RSSSumBytes != nil {
		t.Fatal("unavailable unit produced zero totals")
	}
}

func TestHybridHierarchyDoesNotMixUnrelatedCounters(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "v2")
	v1 := filepath.Join(root, "cpu")
	groupFile(t, filepath.Join(v2, "app"), "cgroup.procs", "42\n")
	groupFile(t, filepath.Join(v1, "app"), "cgroup.procs", "99\n")
	groupFile(t, filepath.Join(v1, "app"), "cpuacct.usage", "9000000000\n")
	c := &processCollector{mounts: []cgroupMount{{path: v2, root: "/", v2: true}, {path: v1, root: "/", controllers: map[string]bool{"cpuacct": true}}}}
	if _, err := c.groupPIDs("/app", true); err != nil {
		t.Fatal(err)
	}
	counters, _, _, _, _ := c.groupMetrics("/app")
	if _, ok := counters["cpu_time_ns"]; ok {
		t.Fatal("mixed v1 counters into v2 membership")
	}
	// On v1, controller hierarchies must contain the same members as the selected hierarchy.
	c.mounts[0].v2 = false
	c.mounts[0].controllers = map[string]bool{"name=systemd": true}
	if _, err := c.groupPIDs("/app", true); err != nil {
		t.Fatal(err)
	}
	counters, _, _, _, issues := c.groupMetrics("/app")
	if _, ok := counters["cpu_time_ns"]; ok || len(issues) == 0 {
		t.Fatal("mixed differing v1 membership")
	}
	groupFile(t, filepath.Join(v1, "app"), "cgroup.procs", "42\n")
	counters, _, _, _, _ = c.groupMetrics("/app")
	if counters["cpu_time_ns"] != 9000000000 {
		t.Fatal("matching v1 controller not used")
	}
}

func TestSystemdVanishedCgroupIsStopped(t *testing.T) {
	root := t.TempDir()
	c := &processCollector{root: root, clock: 100, mounts: []cgroupMount{{path: root, root: "/", v2: true}},
		unitLookup: func(string) (string, error) { return "/gone", nil }}
	cfg := config{processes: true, processLimit: 10, services: []serviceConfig{{Name: "app", Match: serviceMatch{SystemdUnit: "app.service"}}}}
	s, _ := collectFixture(c, cfg, time.Now())
	if s.Processes.Services[0].State != "stopped" || !s.Processes.Services[0].Complete {
		t.Fatalf("vanished systemd cgroup: %+v", s.Processes.Services[0])
	}
}
