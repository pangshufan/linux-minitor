package main

import "time"

type processMetric struct {
	ServiceKeys              []string          `json:"service_keys,omitempty"`
	ParentKey                string            `json:"parent_key,omitempty"`
	WorkingSetBytes          *uint64           `json:"working_set_bytes,omitempty"`
	HandleCount              *uint32           `json:"handle_count,omitempty"`
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
