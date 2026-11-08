package main

import "strings"

// Prefer kernel group counters. Procfs sums are valid only for an unchanged,
// fully observed member set throughout the calculation window.
func calculateServiceRates(w *metricWindow, current rawSample, history []rawSample, base rawSample) {
	if len(current.services) == 0 {
		return
	}
	w.ServiceStatus = map[string]map[string]string{}
	for key, svc := range current.services {
		rates := map[string]float64{}
		states := map[string]string{}
		stable := svc.complete
		check := func(other rawSample) {
			previous, ok := other.services[key]
			if !ok || !previous.complete || len(previous.members) != len(svc.members) {
				stable = false
				return
			}
			for i, name := range svc.members {
				if previous.members[i] != name {
					stable = false
					return
				}
			}
		}
		check(base)
		for _, sample := range history {
			if !sample.at.Before(base.at) {
				check(sample)
			}
		}
		for _, field := range []string{"cpu_time_ns", "read_bytes", "write_bytes"} {
			source := "io"
			if field == "cpu_time_ns" {
				source = "cpu"
			}
			if strings.HasPrefix(svc.sources[source], "cgroup_") {
				if value, ok := w.Rates[svc.counterKey][field]; ok {
					rates[field] = value
					states[field] = "ok"
				} else {
					states[field] = "insufficient_or_reset"
				}
				continue
			}
			if !stable {
				states[field] = "membership_changed_or_incomplete"
				continue
			}
			sum := 0.0
			complete := true
			for _, member := range svc.members {
				value, ok := w.Rates[member][field]
				if !ok {
					complete = false
					break
				}
				sum += value
			}
			if complete {
				rates[field] = sum
				states[field] = "ok"
			} else {
				states[field] = "insufficient_or_reset"
			}
		}
		if cpu, ok := rates["cpu_time_ns"]; ok {
			rates["cpu_percent"] = cpu / 10000000
		}
		if len(rates) > 0 {
			w.Rates[key] = rates
		}
		w.ServiceStatus[key] = states
	}
	// Cgroup counter identities are internal continuity keys, not public service IDs.
	for key := range w.Rates {
		if strings.HasPrefix(key, "cgroup/") {
			delete(w.Rates, key)
		}
	}
}
