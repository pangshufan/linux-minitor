package main

import (
	"testing"
	"time"
)

func TestWindowUsesActualElapsedTime(t *testing.T) {
	start := time.Unix(100, 0)
	base := rawSample{at: start, counters: map[string]map[string]uint64{"net/eth0": {"rx_bytes": 100}}}
	current := rawSample{at: start.Add(1200 * time.Millisecond), counters: map[string]map[string]uint64{"net/eth0": {"rx_bytes": 700}}}
	w := calculateWindow(current, []rawSample{base}, time.Second, time.Second)
	if w.Status != "ok" || w.ElapsedMS != 1200 || w.Rates["net/eth0"]["rx_bytes"] != 500 {
		t.Fatalf("unexpected window: %+v", w)
	}
}

func TestWindowDropsCounterReset(t *testing.T) {
	start := time.Unix(100, 0)
	sample := func(second int, value uint64) rawSample {
		return rawSample{
			at:       start.Add(time.Duration(second) * time.Second),
			counters: map[string]map[string]uint64{"disk/8:0/sda": {"reads": value}},
		}
	}
	history := []rawSample{sample(0, 100), sample(1, 3)}
	w := calculateWindow(sample(2, 130), history, 2*time.Second, time.Second)
	if w.Status != "ok" || len(w.Rates) != 0 {
		t.Fatalf("rate should be absent after a reset: %+v", w)
	}
	w = calculateWindow(sample(2, 130), history[1:], 2*time.Second, time.Second)
	if w.Status != "insufficient_samples" {
		t.Fatalf("window should still be warming up: %+v", w)
	}
}

func TestDiskBottleneckRates(t *testing.T) {
	start := time.Unix(100, 0)
	base := rawSample{at: start, counters: map[string]map[string]uint64{"disk/8:0/sda": {
		"reads": 0, "writes": 0, "read_time_ms": 0, "write_time_ms": 0, "io_time_ms": 0, "weighted_io_time_ms": 0,
	}}}
	current := rawSample{at: start.Add(2 * time.Second), counters: map[string]map[string]uint64{"disk/8:0/sda": {
		"reads": 100, "writes": 20, "read_time_ms": 250, "write_time_ms": 100, "io_time_ms": 1800, "weighted_io_time_ms": 5000,
	}}}
	w := calculateWindow(current, []rawSample{base}, 2*time.Second, time.Second)
	rates := w.Rates["disk/8:0/sda"]
	for key, want := range map[string]float64{"util_percent": 90, "avg_queue_depth": 2.5, "read_await_ms": 2.5, "write_await_ms": 5} {
		if rates[key] != want {
			t.Fatalf("%s = %v, want %v", key, rates[key], want)
		}
	}
	idle := base
	idle.at = start.Add(time.Second)
	w = calculateWindow(idle, []rawSample{base}, time.Second, time.Second)
	if _, ok := w.Rates["disk/8:0/sda"]["read_await_ms"]; ok {
		t.Fatal("zero completed reads must not generate a false zero latency")
	}
	if w.Rates["disk/8:0/sda"]["util_percent"] != 0 {
		t.Fatal("idle util should be zero")
	}
}
