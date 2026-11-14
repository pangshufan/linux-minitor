package main

import (
	"context"
	"strings"
	"time"
)

type metricSnapshot struct {
	Processes        *processSnapshot             `json:"processes,omitempty"`
	SampleID         uint64                       `json:"sample_id"`
	CollectedAt      time.Time                    `json:"collected_at"`
	SampleIntervalMS float64                      `json:"sample_interval_ms"`
	BootedAt         *time.Time                   `json:"booted_at,omitempty"`
	UptimeSeconds    *float64                     `json:"uptime_seconds,omitempty"`
	LoadAverage      []float64                    `json:"load_average_1m_5m_15m,omitempty"`
	MemoryBytes      map[string]uint64            `json:"memory_bytes,omitempty"`
	MemoryAvailable  string                       `json:"memory_available_source"`
	Gauges           map[string]map[string]uint64 `json:"gauges,omitempty"`
	Windows          []metricWindow               `json:"windows"`
	Issues           map[string]string            `json:"issues,omitempty"`
}

type metricWindow struct {
	ServiceStatus map[string]map[string]string  `json:"service_status,omitempty"`
	RequestedMS   float64                       `json:"requested_ms"`
	Start         *time.Time                    `json:"window_start,omitempty"`
	End           time.Time                     `json:"window_end"`
	ElapsedMS     float64                       `json:"elapsed_ms,omitempty"`
	Status        string                        `json:"status"`
	Rates         map[string]map[string]float64 `json:"rates,omitempty"`
	CPU           map[string]map[string]float64 `json:"cpu_percent,omitempty"`
}

type rawSample struct {
	services map[string]rawService
	at       time.Time
	cpu      map[string][]uint64
	counters map[string]map[string]uint64
}

func (a *agent) collectMetrics(ctx context.Context, c config) {
	var id uint64
	processes := newProcessCollector()
	defer processes.close()
	longest := c.sample
	for _, window := range c.windows {
		longest = max(longest, window)
	}
	history := make([]rawSample, 0, int(longest/c.sample)+2)
	periodically(ctx, c.sample, true, func() {
		id++
		s, raw := readMetrics(id)
		processes.collect(c, s, &raw)
		if len(history) > 0 {
			s.SampleIntervalMS = millis(raw.at.Sub(history[len(history)-1].at))
		}
		for _, window := range c.windows {
			s.Windows = append(s.Windows, calculateWindow(raw, history, window, c.sample))
		}
		if len(history) == cap(history) {
			copy(history, history[1:])
			history = history[:len(history)-1]
		}
		history = append(history, raw)
		a.metrics.Store(s)
	})
}

func millis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func calculateWindow(current rawSample, history []rawSample, requested, sample time.Duration) metricWindow {
	w := metricWindow{RequestedMS: millis(requested), End: current.at, Status: "insufficient_samples"}
	if len(history) == 0 {
		return w
	}
	base := history[len(history)-1]
	if requested != sample {
		target := current.at.Add(-requested)
		for i := len(history) - 2; i >= 0; i-- {
			if distance(history[i].at.Sub(target)) >= distance(base.at.Sub(target)) {
				break
			}
			base = history[i]
		}
		if distance(base.at.Sub(target)) > sample/2 {
			return w
		}
	}

	elapsed := current.at.Sub(base.at)
	if elapsed <= 0 {
		return w
	}
	w.Status, w.Start, w.ElapsedMS = "ok", &base.at, millis(elapsed)
	w.Rates = make(map[string]map[string]float64)
	for group, counters := range current.counters {
		rates := make(map[string]float64)
		for name, value := range counters {
			previous, ok := base.counters[group][name]
			if !ok || value < previous {
				continue
			}
			// A missing or reset counter inside a longer window invalidates its rate.
			if requested != sample && !counterContinuous(history, base.at, group, name, value) {
				continue
			}
			rates[name] = float64(value-previous) / elapsed.Seconds()
		}
		if strings.HasPrefix(group, "process/") {
			if cpu, ok := rates["cpu_time_ns"]; ok {
				rates["cpu_percent"] = cpu / 10000000
			}
		}
		if len(rates) > 0 {
			w.Rates[group] = rates
		}
	}
	w.CPU = make(map[string]map[string]float64)
	for name, counters := range current.cpu {
		previous := base.cpu[name]
		if len(previous) != len(counters) {
			continue
		}
		if requested != sample && !cpuContinuous(history, base.at, name, counters) {
			continue
		}
		if usage := cpuPercent(counters, previous); usage != nil {
			w.CPU[name] = usage
		}
	}
	calculateServiceRates(&w, current, history, base)
	return w
}

func distance(d time.Duration) time.Duration {
	return d.Abs()
}

func counterContinuous(history []rawSample, base time.Time, group, key string, next uint64) bool {
	for i := len(history) - 1; i >= 0 && !history[i].at.Before(base); i-- {
		value, ok := history[i].counters[group][key]
		if !ok || value > next {
			return false
		}
		next = value
	}
	return true
}

func cpuContinuous(history []rawSample, base time.Time, key string, next []uint64) bool {
	for i := len(history) - 1; i >= 0 && !history[i].at.Before(base); i-- {
		values := history[i].cpu[key]
		if len(values) != len(next) {
			return false
		}
		for j, value := range values {
			if value > next[j] {
				return false
			}
		}
		next = values
	}
	return true
}

func cpuPercent(current, previous []uint64) map[string]float64 {
	names := []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}
	var total uint64
	delta := make([]uint64, len(names))
	for i := range names {
		if current[i] < previous[i] {
			if names[i] == "iowait" {
				continue
			}
			return nil
		}
		delta[i] = current[i] - previous[i]
		total += delta[i]
	}
	if total == 0 {
		return nil
	}
	values := make(map[string]float64, len(names)+1)
	for i, name := range names {
		values[name] = 100 * float64(delta[i]) / float64(total)
	}
	values["busy"] = 100 - values["idle"] - values["iowait"]
	return values
}

func setValue(groups map[string]map[string]uint64, group, key string, value uint64) {
	if groups[group] == nil {
		groups[group] = make(map[string]uint64)
	}
	groups[group][key] = value
}
