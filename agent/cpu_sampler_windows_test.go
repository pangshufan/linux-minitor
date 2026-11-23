package main

import (
	"math"
	"testing"
	"time"
	"unsafe"
)

func TestPDHCPUABI(t *testing.T) {
	var raw pdhRawCounter
	var value pdhFormatted
	if unsafe.Sizeof(raw) != 40 || unsafe.Offsetof(raw.First) != 16 || unsafe.Sizeof(value) != 16 || unsafe.Offsetof(value.Value) != 8 {
		t.Fatal("PDH structure layout does not match the Windows x64 ABI")
	}
}

func TestPDHCPUWindowNative(t *testing.T) {
	sampler := newPlatformCPUSampler(config{windowsCPUMode: "auto"})
	defer sampler.close()
	if sampler.query == 0 || sampler.handles[pdhTime] == 0 {
		t.Fatalf("native PDH time counter not available: %v", sampler.initIssues)
	}
	base := rawSample{at: time.Now()}
	sampler.collect(&base)
	time.Sleep(1100 * time.Millisecond)
	current := rawSample{at: time.Now()}
	sampler.collect(&current)
	a := current.cpuNative.(*windowsCPUSample)
	b := base.cpuNative.(*windowsCPUSample)
	value, err := calculatePDHWindow(a, b, []rawSample{base}, base, pdhTime)
	if err != nil || value == nil || math.IsNaN(*value) || *value < 0 || *value > 100.1 {
		t.Fatalf("native PDH time window: %v %v", value, err)
	}
	w := metricWindow{CPU: map[string]map[string]float64{}}
	calculatePlatformCPU(&w, current, []rawSample{base}, base)
	if w.CPUSelection == nil || w.CPUSelection.Actual == "" || w.CPU["cpu"]["busy"] < 0 || w.CPU["cpu"]["busy"] > 100 {
		t.Fatalf("native CPU selection: %+v", w)
	}
	// Inserting an unavailable intermediate sample invalidates the whole long window.
	middle := rawSample{at: base.at.Add(500 * time.Millisecond), cpuNative: &windowsCPUSample{sampler: sampler}}
	if _, err := calculatePDHWindow(a, b, []rawSample{base, middle}, base, pdhTime); err == nil {
		t.Fatal("PDH window silently ignored a collection gap")
	}
}
