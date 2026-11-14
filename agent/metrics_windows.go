package main

import (
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	getSystemTimes               = kernel32.NewProc("GetSystemTimes")
	getActiveProcessorGroupCount = kernel32.NewProc("GetActiveProcessorGroupCount")
	setThreadGroupAffinity       = kernel32.NewProc("SetThreadGroupAffinity")
	globalMemoryStatusEx         = kernel32.NewProc("GlobalMemoryStatusEx")
	getTickCount64               = kernel32.NewProc("GetTickCount64")
)

type windowsMemoryStatus struct {
	Length, Load                                             uint32
	TotalPhysical, AvailablePhysical                         uint64
	TotalCommit, AvailableCommit                             uint64
	TotalVirtual, AvailableVirtual, AvailableExtendedVirtual uint64
}

type windowsGroupAffinity struct {
	Mask     uintptr
	Group    uint16
	Reserved [3]uint16
}

func filetimeTicks(t windows.Filetime) uint64 {
	return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime)
}

func readWindowsMemory() (windowsMemoryStatus, error) {
	var info windowsMemoryStatus
	info.Length = uint32(unsafe.Sizeof(info))
	ok, _, err := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return info, err
	}
	return info, nil
}

func readWindowsCPU() ([]uint64, error) {
	var totals [8]uint64
	groupCount, _, err := getActiveProcessorGroupCount.Call()
	if groupCount == 0 {
		return nil, err
	}
	if groupCount == 1 {
		if err := accumulateWindowsCPU(&totals); err != nil {
			return nil, err
		}
		return totals[:], nil
	}

	// Kernel CPU time includes idle time. GetSystemTimes only covers the
	// calling thread's primary group on hosts with multiple groups.
	// Use a dedicated thread which exits after the scan, avoiding affinity
	// changes on an OS thread returned to the Go scheduler.
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		for group := uint16(0); group < uint16(groupCount); group++ {
			n := windows.GetActiveProcessorCount(group)
			if n == 0 || n > 64 {
				result <- fmt.Errorf("invalid processor count for group %d", group)
				return
			}
			affinity := windowsGroupAffinity{Mask: uintptr(uint64(1)<<n - 1), Group: group}
			ok, _, err := setThreadGroupAffinity.Call(uintptr(windows.CurrentThread()), uintptr(unsafe.Pointer(&affinity)), 0)
			if ok == 0 {
				result <- err
				return
			}
			if err := accumulateWindowsCPU(&totals); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	}()
	if err := <-result; err != nil {
		return nil, err
	}
	return totals[:], nil
}

func accumulateWindowsCPU(totals *[8]uint64) error {
	var idle, kernel, user windows.Filetime
	ok, _, err := getSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if ok == 0 {
		return err
	}
	i, k, u := filetimeTicks(idle), filetimeTicks(kernel), filetimeTicks(user)
	if k < i {
		return fmt.Errorf("kernel CPU time is below idle time")
	}
	totals[0] += u
	totals[2] += k - i
	totals[3] += i
	return nil
}

func windowsUptime() time.Duration {
	ms, _, _ := getTickCount64.Call()
	return time.Duration(ms) * time.Millisecond
}

func readMetrics(id uint64) (*metricSnapshot, rawSample) {
	now := time.Now()
	uptime := windowsUptime()
	boot := now.Add(-uptime).UTC()
	seconds := uptime.Seconds()
	s := &metricSnapshot{SampleID: id, CollectedAt: now, BootedAt: &boot, UptimeSeconds: &seconds,
		MemoryAvailable: "unavailable", Issues: map[string]string{}}
	raw := rawSample{at: now, cpu: map[string][]uint64{}, counters: map[string]map[string]uint64{}}
	if cpu, err := readWindowsCPU(); err == nil {
		raw.cpu["cpu"] = cpu
	} else {
		s.Issues["cpu"] = err.Error()
	}
	if memory, err := readWindowsMemory(); err == nil && memory.AvailablePhysical <= memory.TotalPhysical && memory.TotalPhysical > 0 {
		s.MemoryBytes = map[string]uint64{"total": memory.TotalPhysical, "available": memory.AvailablePhysical, "used": memory.TotalPhysical - memory.AvailablePhysical}
		s.MemoryAvailable = "GlobalMemoryStatusEx"
	} else if err != nil {
		s.Issues["memory"] = err.Error()
	} else {
		s.Issues["memory"] = "invalid physical memory counters"
	}
	if err := windowsInterfaces(windows.MibIfTableNormal, func(row *windows.MibIfRow2) {
		raw.counters[windowsInterfaceKey(row)] = map[string]uint64{
			"rx_bytes": row.InOctets, "tx_bytes": row.OutOctets,
			"rx_packets": row.InUcastPkts + row.InNUcastPkts, "tx_packets": row.OutUcastPkts + row.OutNUcastPkts,
			"rx_errors": row.InErrors, "tx_errors": row.OutErrors, "rx_drop": row.InDiscards, "tx_drop": row.OutDiscards,
		}
	}); err != nil {
		s.Issues["network"] = err.Error()
	}
	return s, raw
}

func windowsInterfaces(level uint32, use func(*windows.MibIfRow2)) error {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(level, &table); err != nil {
		return err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	rows := unsafe.Slice(&table.Table[0], int(table.NumEntries))
	for i := range rows {
		use(&rows[i])
	}
	return nil
}

func windowsInterfaceKey(row *windows.MibIfRow2) string {
	return fmt.Sprintf("net/%d/%s", row.InterfaceLuid, windows.UTF16ToString(row.Alias[:]))
}
