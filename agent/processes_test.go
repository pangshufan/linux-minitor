package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func processStat(name string, start, ticks uint64) string {
	return fmt.Sprintf("42 (%s) S 1 0 0 0 0 0 10 0 2 0 %d 0 0 0 0 0 3 0 %d 4096 2", name, ticks, start)
}
func TestProcessIdentityAndPageSize(t *testing.T) {
	p, err := parseProcessStat(42, processStat("worker (one)", 300, 120), 65536)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "worker (one)" || p.PPID != 1 || p.Threads != 3 || p.RSSBytes != 131072 || p.Key != "process/42/300" || p.ticks != 120 {
		t.Fatalf("unexpected: %+v", p)
	}
	if _, err := parseProcessStat(42, "42 (broken) S", 4096); err == nil {
		t.Fatal("accepted incomplete stat")
	}
}
func TestProcessCollectionAndPIDReuse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "42")
	os.MkdirAll(filepath.Join(path, "fd"), 0700)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", processStat("worker", 300, 120))
	write("status", "Uid:\t1000 1000 1000 1000\nvoluntary_ctxt_switches: 20\n")
	write("cmdline", "java\x00-Xmx2g\x00worker\x00")
	write("io", "read_bytes: 4096\nwrite_bytes: 8192\n")
	c := &processCollector{root: root, clock: 250, previous: map[string]uint64{}}
	cfg := config{processes: true, processLimit: 10, processNames: "worker"}
	boot := time.Unix(1000, 0)
	up := 10.0
	sample := func(at time.Time) (*metricSnapshot, rawSample) {
		s := &metricSnapshot{BootedAt: &boot, UptimeSeconds: &up, MemoryBytes: map[string]uint64{"total": 1000000}}
		r := rawSample{at: at, counters: map[string]map[string]uint64{}}
		c.collect(cfg, s, &r)
		return s, r
	}
	now := time.Now()
	s, first := sample(now)
	if len(s.Processes.Items) != 1 || *s.Processes.Items[0].UID != 1000 || s.Processes.ClockTicks != 250 {
		t.Fatalf("unexpected: %+v", s.Processes)
	}
	write("stat", processStat("worker", 300, 370))
	_, second := sample(now.Add(time.Second))
	w := calculateWindow(second, []rawSample{first}, time.Second, time.Second)
	if w.Rates["process/42/300"]["cpu_percent"] != 100 {
		t.Fatalf("CPU: %+v", w)
	}
	write("stat", processStat("worker", 400, 1))
	_, third := sample(now.Add(2 * time.Second))
	w = calculateWindow(third, []rawSample{second}, time.Second, time.Second)
	if _, ok := w.Rates["process/42/400"]["cpu_percent"]; ok {
		t.Fatal("PID reuse created rate")
	}
	cfg.processMatch = "worker,absent"
	s, _ = sample(now)
	if s.Processes.Matches["worker"] != 1 || s.Processes.Matches["absent"] != 0 || s.Processes.Items[0].JVMMaxHeapBytes == nil || *s.Processes.Items[0].JVMMaxHeapBytes != 2<<30 || s.Processes.Items[0].IOCounters["read_bytes"] != 4096 {
		t.Fatalf("script metric mapping: %+v", s.Processes)
	}
	cfg.processNames = "other"
	s, _ = sample(now)
	if s.Processes.Matched != 0 {
		t.Fatal("name filter ignored")
	}
}

func TestJVMHeapOptions(t *testing.T) {
	for _, tt := range []struct {
		cmd  string
		want uint64
	}{
		{"java\x00-Xmx512m\x00", 512 << 20}, {"java\x00-Xmx1g\x00-Xmx2G\x00", 2 << 30}, {"java\x00-Dnote=-Xmx2g\x00", 0}, {"java\x00-Xmx18446744073709551615t\x00", 0},
	} {
		value, _ := parseJVMHeap([]byte(tt.cmd))
		if tt.want == 0 {
			if value != nil {
				t.Fatal("invalid Xmx accepted")
			}
		} else if value == nil || *value != tt.want {
			t.Fatalf("heap=%v", value)
		}
	}
}
