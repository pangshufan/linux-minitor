//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

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
