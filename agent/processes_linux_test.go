package main

import (
	"os"
	"strconv"
	"testing"
)

func TestLinuxProcessSources(t *testing.T) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	self, err := parseProcessStat(os.Getpid(), string(data), uint64(os.Getpagesize()))
	if err != nil {
		t.Fatal(err)
	}
	collector := newProcessCollector()
	defer collector.close()
	if collector.clock == 0 {
		t.Fatal("AT_CLKTCK not found")
	}
	snapshot, raw := readMetrics(1)
	collector.collect(config{processes: true, processNames: self.Name, processLimit: 10000}, snapshot, &raw)
	for _, p := range snapshot.Processes.Items {
		if p.PID == os.Getpid() {
			if p.FDCount == nil || p.UID == nil || p.IOCounters == nil || p.StartedAt == nil {
				t.Fatalf("incomplete self metrics: %+v", p)
			}
			return
		}
	}
	t.Fatal("current process not found: " + strconv.Itoa(os.Getpid()))
}
