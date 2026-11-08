package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Process identity includes start ticks because Linux reuses PIDs.
type processMetric struct {
	ServiceKeys              []string          `json:"service_keys,omitempty"`
	ParentKey                string            `json:"parent_key,omitempty"`
	PSSBytes                 *uint64           `json:"pss_bytes,omitempty"`
	PSSAt                    *time.Time        `json:"pss_at,omitempty"`
	MatchedPatterns          []string          `json:"matched_patterns,omitempty"`
	IOCounters               map[string]uint64 `json:"io_counters,omitempty"`
	JVMMaxHeapBytes          *uint64           `json:"jvm_max_heap_bytes,omitempty"`
	JVMMaxHeapOption         string            `json:"jvm_max_heap_option,omitempty"`
	Key                      string            `json:"key"`
	PID                      int               `json:"pid"`
	PPID                     uint64            `json:"ppid"`
	Name                     string            `json:"name"`
	State                    string            `json:"state"`
	UID                      *uint64           `json:"uid,omitempty"`
	Threads                  uint64            `json:"threads"`
	StartTicks               uint64            `json:"start_ticks"`
	StartedAt                *time.Time        `json:"started_at,omitempty"`
	UptimeSeconds            *float64          `json:"uptime_seconds,omitempty"`
	VirtualBytes             uint64            `json:"virtual_bytes"`
	RSSBytes                 uint64            `json:"rss_bytes"`
	MemoryPercent            *float64          `json:"memory_percent,omitempty"`
	FDCount                  *int              `json:"fd_count,omitempty"`
	Issues                   map[string]string `json:"issues,omitempty"`
	ticks                    uint64
	minorFaults, majorFaults uint64
}

type processSnapshot struct {
	Services        []serviceSnapshot `json:"services"`
	LifecycleSource string            `json:"lifecycle_source"`
	Matches         map[string]int    `json:"matches,omitempty"`
	Enabled         bool              `json:"enabled"`
	ClockTicks      uint64            `json:"clock_ticks_per_second,omitempty"`
	Matched         int               `json:"matched"`
	Reported        int               `json:"reported"`
	Truncated       bool              `json:"truncated"`
	Items           []processMetric   `json:"items"`
	Issues          map[string]string `json:"issues,omitempty"`
}

type processCollector struct {
	groupMemberships map[string]groupMembership
	asyncUnits       bool
	root             string
	clock            uint64
	previous         map[string]uint64
	services         map[string]*serviceTracker
	mounts           []cgroupMount
	unitLookup       func(string) (string, error)
	pidfds           map[string]int
	pidfdUnavailable bool
	pss              map[string]pssCache
	index            map[int]processMetric
	indexAt          time.Time
	indexErr         error
}

func newProcessCollector() *processCollector {
	// Linux exposes _SC_CLK_TCK as AT_CLKTCK in the auxiliary vector; never assume HZ=100.
	data, _ := os.ReadFile("/proc/self/auxv")
	word := strconv.IntSize / 8
	read := func(b []byte) uint64 {
		if word == 8 {
			return binary.NativeEndian.Uint64(b)
		}
		return uint64(binary.NativeEndian.Uint32(b))
	}
	var clock uint64
	for i := 0; i+2*word <= len(data); i += 2 * word {
		if read(data[i:]) == 17 {
			clock = read(data[i+word:])
			break
		}
	}
	return &processCollector{root: "/proc", clock: clock, previous: make(map[string]uint64)}
}

func parseProcessStat(pid int, data string, page uint64) (processMetric, error) {
	p := processMetric{PID: pid}
	left, right := strings.IndexByte(data, '('), strings.LastIndexByte(data, ')')
	if left < 0 || right <= left {
		return p, fmt.Errorf("invalid process stat")
	}
	p.Name = data[left+1 : right]
	f := strings.Fields(data[right+1:])
	if len(f) < 22 {
		return p, fmt.Errorf("short process stat")
	}
	p.State = f[0]
	nums := make(map[int]uint64)
	for _, i := range []int{1, 7, 9, 11, 12, 17, 19, 20} {
		v, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return p, err
		}
		nums[i] = v
	}
	rss, err := strconv.ParseInt(f[21], 10, 64)
	if err != nil {
		return p, err
	}
	p.PPID, p.Threads, p.StartTicks, p.VirtualBytes = nums[1], nums[17], nums[19], nums[20]
	p.ticks, p.minorFaults, p.majorFaults = nums[11]+nums[12], nums[7], nums[9]
	if rss > 0 {
		p.RSSBytes = uint64(rss) * page
	}
	p.Key = fmt.Sprintf("process/%d/%d", pid, p.StartTicks)
	return p, nil
}

func (c *processCollector) enrich(cfg config, s *metricSnapshot, raw *rawSample, out *processSnapshot, candidates []processMetric) {
	next := map[string]uint64{}
	for _, p := range candidates {
		next[p.Key] = p.ticks
	}
	delta := func(p processMetric) uint64 {
		if old, ok := c.previous[p.Key]; ok && p.ticks >= old {
			return p.ticks - old
		}
		return 0
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := delta(candidates[i]), delta(candidates[j])
		if a != b {
			return a > b
		}
		if candidates[i].RSSBytes != candidates[j].RSSBytes {
			return candidates[i].RSSBytes > candidates[j].RSSBytes
		}
		return candidates[i].PID < candidates[j].PID
	})
	c.previous = next
	out.Matched = len(candidates)
	if len(candidates) > cfg.processLimit {
		candidates = candidates[:cfg.processLimit]
		out.Truncated = true
	}
	for _, p := range candidates {
		path := filepath.Join(c.root, strconv.Itoa(p.PID))
		p.Issues = make(map[string]string)
		{
			if cmd, err := readProcessCommand(filepath.Join(path, "cmdline")); err == nil {
				p.JVMMaxHeapBytes, p.JVMMaxHeapOption = parseJVMHeap(cmd)
			} else {
				p.Issues["cmdline"] = err.Error()
			}
		}
		counters := map[string]uint64{"minor_faults": p.minorFaults, "major_faults": p.majorFaults}
		if c.clock > 0 {
			counters["cpu_time_ns"] = p.ticks/c.clock*1000000000 + p.ticks%c.clock*1000000000/c.clock
			if s.BootedAt != nil {
				at := s.BootedAt.Add(time.Duration(float64(p.StartTicks) / float64(c.clock) * float64(time.Second)))
				p.StartedAt = &at
			}
			if s.UptimeSeconds != nil {
				age := *s.UptimeSeconds - float64(p.StartTicks)/float64(c.clock)
				if age >= 0 {
					p.UptimeSeconds = &age
				}
			}
		}
		if total := s.MemoryBytes["total"]; total > 0 {
			pct := 100 * float64(p.RSSBytes) / float64(total)
			p.MemoryPercent = &pct
		}
		if data, err := os.ReadFile(filepath.Join(path, "status")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				f := strings.Fields(line)
				if len(f) < 2 {
					continue
				}
				value, err := strconv.ParseUint(f[1], 10, 64)
				if err != nil {
					continue
				}
				switch f[0] {
				case "Uid:":
					p.UID = &value
				case "voluntary_ctxt_switches:":
					counters["voluntary_context_switches"] = value
				case "nonvoluntary_ctxt_switches:":
					counters["involuntary_context_switches"] = value
				}
			}
		} else {
			p.Issues["status"] = err.Error()
		}
		if data, err := os.ReadFile(filepath.Join(path, "io")); err == nil {
			p.IOCounters = map[string]uint64{}
			for _, line := range strings.Split(string(data), "\n") {
				f := strings.Fields(line)
				if len(f) != 2 {
					continue
				}
				value, err := strconv.ParseUint(f[1], 10, 64)
				if err == nil {
					counters[strings.TrimSuffix(f[0], ":")] = value
					p.IOCounters[strings.TrimSuffix(f[0], ":")] = value
				}
			}
		} else {
			p.Issues["io"] = err.Error()
		}
		if count, err := countProcessFDs(filepath.Join(path, "fd")); err == nil {
			p.FDCount = &count
		} else {
			p.Issues["fd"] = err.Error()
		}
		// Revalidate after the supplementary reads so PID reuse cannot mix two processes.
		data, err := os.ReadFile(filepath.Join(path, "stat"))
		if err != nil {
			continue
		}
		check, err := parseProcessStat(p.PID, string(data), uint64(os.Getpagesize()))
		if err != nil || check.Key != p.Key {
			continue
		}
		if cfg.processPSS {
			cached, ok := c.pss[p.Key]
			if !ok || s.CollectedAt.Sub(cached.at) >= cfg.processPSSInterval {
				value, err := readProcessPSS(path)
				cached = pssCache{at: s.CollectedAt, value: value, err: err}
				if c.pss == nil {
					c.pss = map[string]pssCache{}
				}
				c.pss[p.Key] = cached
			}
			if cached.err != nil {
				p.Issues["pss"] = cached.err.Error()
			} else {
				p.PSSBytes = &cached.value
				p.PSSAt = &cached.at
			}
		}
		raw.counters[p.Key] = counters
		out.Items = append(out.Items, p)
	}
	out.Reported = len(out.Items)
}

// Bound command line reads and discard the raw bytes after matching/extracting Xmx.
func readProcessCommand(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if len(data) > 1024*1024 {
		return nil, fmt.Errorf("command line exceeds 1 MiB")
	}
	return data, err
}

var xmxOption = regexp.MustCompile(`^-Xmx([0-9]+)([kKmMgGtT]?)$`)

func parseJVMHeap(command []byte) (*uint64, string) {
	var result *uint64
	var option string
	for _, arg := range strings.Split(string(command), "\x00") {
		parts := xmxOption.FindStringSubmatch(arg)
		if parts == nil {
			continue
		}
		n, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		shift := uint(0)
		switch strings.ToLower(parts[2]) {
		case "k":
			shift = 10
		case "m":
			shift = 20
		case "g":
			shift = 30
		case "t":
			shift = 40
		}
		if n > ^uint64(0)>>shift {
			continue
		}
		value := n << shift
		result = &value
		option = arg
	}
	return result, option
}

func countProcessFDs(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	count := 0
	for {
		names, err := f.Readdirnames(256)
		count += len(names)
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return 0, err
		}
	}
}
