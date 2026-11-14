package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsHostAndProcessCollection(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := config{processes: true, processLimit: 32, services: []serviceConfig{
		{Name: "self", Match: serviceMatch{Executable: exe}},
	}}
	version, err := checkPlatform(c)
	if err != nil {
		t.Fatal(err)
	}
	s, raw := readMetrics(1)
	if s.MemoryBytes["total"] == 0 || s.BootedAt == nil || len(raw.cpu["cpu"]) != 8 {
		t.Fatalf("host collectors unavailable: %+v", s.Issues)
	}
	newProcessCollector().collect(c, s, &raw)
	target := s.Processes.Services[0]
	if target.State != "running" {
		t.Fatalf("process selector did not find test executable: %+v", target)
	}
	for _, p := range s.Processes.Items {
		if p.PID != os.Getpid() {
			continue
		}
		if p.WorkingSetBytes == nil || *p.WorkingSetBytes == 0 || p.StartedAt == nil || p.HandleCount == nil {
			t.Fatalf("incomplete self metrics: %+v", p.Issues)
		}
		a := &agent{hostID: "windows-test"}
		a.collectInventory(t.Context(), version)
		a.collectFilesystems()
		volume := a.inventory.Load().System["system_volume"]
		for _, mount := range a.filesystems.Load().Mounts {
			if strings.EqualFold(mount.Target, volume) && mount.Capacity != nil && mount.Capacity.TotalBytes > 0 {
				return
			}
		}
		t.Fatalf("system volume %q not collected: %+v", volume, a.filesystems.Load())
	}
	t.Fatal("test PID not reported")
}

func TestWindowsServiceSelectorAndConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "configs", "agent-windows.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "C:/ProgramData/Monitor/") {
		t.Fatal("Windows configuration should use absolute certificate paths")
	}
	rules, err := parseServiceConfig(`[{"name":"Missing","match":{"windows_service":"MonitorAgent_Test_NoSuchService_9413"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	c := config{processes: true, processLimit: 32, services: rules}
	s, raw := readMetrics(1)
	newProcessCollector().collect(c, s, &raw)
	if got := s.Processes.Services[0]; got.State != "unknown" || got.Complete {
		t.Fatalf("missing service reported as stopped: %+v", got)
	}
	rules[0].Match = serviceMatch{SystemdUnit: "demo.service"}
	if _, err := checkPlatform(c); err == nil {
		t.Fatal("Windows accepted a systemd selector")
	}
}

func TestWindowsRatesPreserveCPUUnits(t *testing.T) {
	before := time.Now()
	base := rawSample{at: before, cpu: map[string][]uint64{"cpu": {10, 0, 20, 70, 0, 0, 0, 0}}}
	current := rawSample{at: before.Add(time.Second), cpu: map[string][]uint64{"cpu": {20, 0, 30, 150, 0, 0, 0, 0}}}
	if got := calculateWindow(current, []rawSample{base}, time.Second, time.Second).CPU["cpu"]["busy"]; got != 20 {
		t.Fatalf("busy CPU = %v, want 20", got)
	}
}
