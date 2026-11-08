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
