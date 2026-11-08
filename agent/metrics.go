package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
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

func readMetrics(id uint64) (*metricSnapshot, rawSample) {
	at := time.Now()
	s := &metricSnapshot{
		SampleID: id, CollectedAt: at, Issues: make(map[string]string),
		MemoryAvailable: "unavailable", Gauges: make(map[string]map[string]uint64),
	}
	raw := rawSample{
		at: at, cpu: make(map[string][]uint64),
		counters: make(map[string]map[string]uint64),
	}

	readSource(s.Issues, "/proc/stat", func(fields []string) {
		name := fields[0]
		switch {
		case strings.HasPrefix(name, "cpu") && len(fields) >= 9:
			if values, ok := uints(fields[1:9]); ok {
				raw.cpu[name] = values
			}
		case name == "btime":
			if value, ok := uintAt(fields, 1); ok {
				boot := time.Unix(int64(value), 0).UTC()
				s.BootedAt = &boot
			}
		case name == "ctxt" || name == "intr" || name == "processes":
			if value, ok := uintAt(fields, 1); ok {
				setValue(raw.counters, "system", name, value)
			}
		case name == "procs_running" || name == "procs_blocked":
			if value, ok := uintAt(fields, 1); ok {
				setValue(s.Gauges, "system", name, value)
			}
		}
	})
	if raw.cpu["cpu"] == nil && s.Issues["/proc/stat"] == "" {
		s.Issues["/proc/stat"] = "aggregate CPU counters unavailable"
	}
	readSource(s.Issues, "/proc/uptime", func(fields []string) {
		if value, err := strconv.ParseFloat(fields[0], 64); err == nil {
			s.UptimeSeconds = &value
		}
	})
	readSource(s.Issues, "/proc/loadavg", func(fields []string) {
		if len(fields) < 3 {
			return
		}
		load := make([]float64, 0, 3)
		for _, field := range fields[:3] {
			value, err := strconv.ParseFloat(field, 64)
			if err != nil {
				return
			}
			load = append(load, value)
		}
		s.LoadAverage = load
	})
	s.MemoryBytes = readMemory(s.Issues)
	if _, ok := s.MemoryBytes["available"]; ok {
		s.MemoryAvailable = "kernel"
	}
	if _, ok := s.MemoryBytes["total"]; !ok && s.Issues["/proc/meminfo"] == "" {
		s.Issues["/proc/meminfo"] = "MemTotal unavailable"
	}
	readSource(s.Issues, "/proc/vmstat", func(f []string) {
		switch f[0] {
		case "pswpin", "pswpout", "pgmajfault":
			if n, ok := uintAt(f, 1); ok {
				setValue(raw.counters, "vm", f[0], n)
			}
		}
	})
	readSource(s.Issues, "/proc/diskstats", func(f []string) {
		if len(f) < 14 {
			return
		}
		group := "disk/" + f[0] + ":" + f[1] + "/" + f[2]
		for _, field := range []struct {
			index int
			name  string
			scale uint64
		}{
			{3, "reads", 1}, {5, "read_bytes", 512}, {6, "read_time_ms", 1},
			{7, "writes", 1}, {9, "write_bytes", 512}, {10, "write_time_ms", 1},
			{14, "discards", 1}, {16, "discard_bytes", 512}, {18, "flushes", 1},
		} {
			if n, ok := uintAt(f, field.index); ok {
				setValue(raw.counters, group, field.name, n*field.scale)
			}
		}
		if n, ok := uintAt(f, 11); ok {
			setValue(s.Gauges, group, "io_in_progress", n)
		}
	})
	readSource(s.Issues, "/proc/net/dev", func(f []string) {
		if len(f) < 17 || !strings.HasSuffix(f[0], ":") {
			return
		}
		group := "net/" + strings.TrimSuffix(f[0], ":")
		for i, name := range []string{"rx_bytes", "rx_packets", "rx_errors", "rx_drop", "", "", "", "", "tx_bytes", "tx_packets", "tx_errors", "tx_drop"} {
			if name != "" {
				if n, ok := uintAt(f, i+1); ok {
					setValue(raw.counters, group, name, n)
				}
			}
		}
	})
	readTCP(s, raw.counters)
	readSource(s.Issues, "/proc/sys/fs/file-nr", func(f []string) {
		if len(f) >= 3 {
			if values, ok := uints(f[:3]); ok {
				setValue(s.Gauges, "system", "file_handles_allocated", values[0])
				setValue(s.Gauges, "system", "file_handles_max", values[2])
			}
		}
	})
	for _, resource := range []string{"cpu", "memory", "io"} {
		readSource(s.Issues, "/proc/pressure/"+resource, func(f []string) {
			for _, item := range f[1:] {
				if key, value, ok := strings.Cut(item, "="); ok && key == "total" {
					if n, err := strconv.ParseUint(value, 10, 64); err == nil {
						if resource != "cpu" || f[0] == "some" {
							setValue(raw.counters, "psi/"+resource, f[0]+"_stall_us", n)
						}
					}
				}
			}
		})
	}
	return s, raw
}

func readMemory(issues map[string]string) map[string]uint64 {
	names := map[string]string{
		"MemTotal:": "total", "MemFree:": "free", "MemAvailable:": "available",
		"Cached:": "cached", "Buffers:": "buffers", "SwapTotal:": "swap_total", "SwapFree:": "swap_free",
	}
	values := make(map[string]uint64)
	readSource(issues, "/proc/meminfo", func(f []string) {
		if name, ok := names[f[0]]; ok && len(f) >= 3 && f[2] == "kB" {
			if n, ok := uintAt(f, 1); ok {
				values[name] = n * 1024
			}
		}
	})
	if total, ok := values["total"]; ok {
		if available, ok := values["available"]; ok && total >= available {
			values["used"] = total - available
		}
	}
	if total, ok := values["swap_total"]; ok {
		if free, ok := values["swap_free"]; ok && total >= free {
			values["swap_used"] = total - free
		}
	}
	return values
}

func readTCP(s *metricSnapshot, counters map[string]map[string]uint64) {
	for _, path := range []string{"/proc/net/snmp", "/proc/net/netstat"} {
		var header []string
		readSource(s.Issues, path, func(f []string) {
			group := strings.TrimSuffix(f[0], ":")
			if group != "Tcp" && group != "TcpExt" && group != "Udp" || len(f) < 2 {
				return
			}
			if header == nil || header[0] != f[0] {
				header = f
				return
			}
			for i := 1; i < min(len(header), len(f)); i++ {
				value, ok := uintAt(f, i)
				if !ok {
					continue
				}
				switch header[i] {
				case "CurrEstab":
					setValue(s.Gauges, "tcp", "established_or_close_wait", value)
				case "RetransSegs", "InSegs", "OutSegs", "ListenOverflows", "ListenDrops", "InErrors", "RcvbufErrors":
					setValue(counters, strings.ToLower(group), header[i], value)
				}
			}
			header = nil
		})
	}
}

func setValue(groups map[string]map[string]uint64, group, key string, value uint64) {
	if groups[group] == nil {
		groups[group] = make(map[string]uint64)
	}
	groups[group][key] = value
}

func readSource(issues map[string]string, path string, handle func([]string)) {
	if err := readFields(path, handle); err != nil {
		issues[path] = err.Error()
	}
}

func readFields(path string, handle func([]string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) > 0 {
			handle(fields)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func uintAt(fields []string, index int) (uint64, bool) {
	if index >= len(fields) {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[index], 10, 64)
	return n, err == nil
}

func uints(fields []string) ([]uint64, bool) {
	numbers := make([]uint64, len(fields))
	for i := range fields {
		n, ok := uintAt(fields, i)
		if !ok {
			return nil, false
		}
		numbers[i] = n
	}
	return numbers, true
}
