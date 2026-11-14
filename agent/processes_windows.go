package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	getProcessMemoryInfo  = kernel32.NewProc("K32GetProcessMemoryInfo")
	getProcessHandleCount = kernel32.NewProc("GetProcessHandleCount")
)

type windowsProcessMemory struct {
	Size, PageFaultCount                                   uint32
	PeakWorkingSet, WorkingSet, PeakPagedPool, PagedPool   uintptr
	PeakNonPagedPool, NonPagedPool, Pagefile, PeakPagefile uintptr
}

type windowsProcesses struct {
	trackers map[string]*serviceLifecycle
}

func newProcessCollector() *windowsProcesses {
	return &windowsProcesses{trackers: map[string]*serviceLifecycle{}}
}

func (*windowsProcesses) close() {}

func listWindowsProcesses() (map[uint32]windows.ProcessEntry32, error) {
	handle, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle)
	result := map[uint32]windows.ProcessEntry32{}
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(handle, &e); err == nil; err = windows.Process32Next(handle, &e) {
		if e.ProcessID != 0 {
			result[e.ProcessID] = e
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return result, nil
}

func queryWindowsService(manager windows.Handle, name string) (windows.SERVICE_STATUS_PROCESS, error) {
	var status windows.SERVICE_STATUS_PROCESS
	if manager == 0 {
		return status, fmt.Errorf("SCM unavailable")
	}
	serviceName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return status, err
	}
	handle, err := windows.OpenService(manager, serviceName, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return status, err
	}
	defer windows.CloseServiceHandle(handle)
	var needed uint32
	err = windows.QueryServiceStatusEx(handle, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed)
	return status, err
}

func openWindowsProcess(pid uint32) (windows.Handle, error) {
	return windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
}

func windowsProcessPath(pid uint32) (string, uint64, error) {
	handle, err := openWindowsProcess(pid)
	if err != nil {
		return "", 0, err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", 0, err
	}
	if filetimeTicks(exited) != 0 {
		return "", 0, fmt.Errorf("process exited")
	}
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size); err != nil {
		return "", 0, err
	}
	return windows.UTF16ToString(buf[:size]), filetimeTicks(created), nil
}

func readWindowsProcess(entry windows.ProcessEntry32, now time.Time) (processMetric, map[string]uint64, error) {
	handle, err := openWindowsProcess(entry.ProcessID)
	if err != nil {
		return processMetric{}, nil, err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return processMetric{}, nil, err
	}
	start := time.Unix(0, created.Nanoseconds()).UTC()
	if filetimeTicks(exited) != 0 || start.After(now) {
		return processMetric{}, nil, fmt.Errorf("process changed during enumeration")
	}
	p := processMetric{PID: int(entry.ProcessID), PPID: uint64(entry.ParentProcessID), Name: windows.UTF16ToString(entry.ExeFile[:]),
		State: "running", Threads: uint64(entry.Threads), StartTicks: filetimeTicks(created), StartedAt: &start, Issues: map[string]string{}}
	p.Key = fmt.Sprintf("process/%d/%d", p.PID, p.StartTicks)
	uptime := now.Sub(start).Seconds()
	p.UptimeSeconds = &uptime
	counters := map[string]uint64{"cpu_time_ns": (filetimeTicks(kernel) + filetimeTicks(user)) * 100}
	mem := windowsProcessMemory{Size: uint32(unsafe.Sizeof(windowsProcessMemory{}))}
	ok, _, err := getProcessMemoryInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&mem)), uintptr(mem.Size))
	if ok == 0 {
		p.Issues["working_set"] = err.Error()
	} else {
		p.RSSBytes = uint64(mem.WorkingSet)
		p.WorkingSetBytes = &p.RSSBytes
	}
	var handles uint32
	if ok, _, _ := getProcessHandleCount.Call(uintptr(handle), uintptr(unsafe.Pointer(&handles))); ok != 0 {
		p.HandleCount = &handles
	}
	return p, counters, nil
}

func (c *windowsProcesses) collect(cfg config, snapshot *metricSnapshot, raw *rawSample) {
	out := &processSnapshot{Enabled: cfg.processes, LifecycleSource: "windows_process_snapshot", Services: []serviceSnapshot{}, Items: []processMetric{}}
	snapshot.Processes = out
	if !cfg.processes || len(cfg.services) == 0 {
		return
	}
	entries, scanErr := listWindowsProcesses()
	if scanErr != nil {
		out.Issues = map[string]string{"process_snapshot": scanErr.Error()}
	}
	children := make(map[uint32][]uint32)
	for id, e := range entries {
		children[e.ParentProcessID] = append(children[e.ParentProcessID], id)
	}
	for parent := range children {
		slices.Sort(children[parent])
	}
	var scm windows.Handle
	for _, target := range cfg.services {
		if target.Match.WindowsService != "" {
			scm, _ = windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
			break
		}
	}
	if scm != 0 {
		defer windows.CloseServiceHandle(scm)
	}
	measured := make(map[uint32]*processMetric)
	// Bound reads across all configured services. Reuse process data for overlapping selectors.
	attempted := make(map[uint32]bool)
	type image struct {
		path    string
		created uint64
		err     error
	}
	paths := make(map[uint32]image)
	raw.services = map[string]rawService{}
	out.Matches = map[string]int{}

	for _, target := range cfg.services {
		key := serviceKey(target.Name)
		svc := serviceSnapshot{Key: key, Name: target.Name, State: "stopped", Complete: true, MembershipSource: "windows_executable",
			ProcessKeys: []string{}, RootKeys: []string{}, Sources: map[string]string{"cpu": "windows_process_times", "rss_sum": "windows_working_set"}, Issues: map[string]string{}}
		roots := []uint32{}
		scmName := target.Match.WindowsService
		if scmName != "" {
			svc.MembershipSource = "windows_scm"
			status, err := queryWindowsService(scm, scmName)
			switch {
			case err != nil:
				svc.State, svc.Complete = "unknown", false
				svc.Issues["scm"] = err.Error()
			case status.CurrentState == windows.SERVICE_STOPPED:
				// SCM confirms stopped even when there is no process snapshot.
			case status.CurrentState == windows.SERVICE_RUNNING:
				svc.State = "running"
				if status.ServiceType&windows.SERVICE_WIN32_SHARE_PROCESS != 0 || status.ServiceFlags&windows.SERVICE_RUNS_IN_SYSTEM_PROCESS != 0 {
					svc.Complete = false
					svc.Issues["shared_process"] = "SCM service uses a shared host process"
				} else if status.ProcessId != 0 {
					roots = append(roots, status.ProcessId)
				} else {
					svc.Complete = false
					svc.Issues["process"] = "running service has no attributable PID"
				}
			default:
				svc.State, svc.Complete = "unknown", false
				svc.Issues["scm"] = fmt.Sprintf("service state %d", status.CurrentState)
			}
		} else if scanErr != nil {
			svc.State, svc.Complete = "unknown", false
			svc.Issues["process_snapshot"] = scanErr.Error()
		} else {
			selectors := target.MatchAny
			if len(selectors) == 0 {
				selectors = []serviceMatch{target.Match}
			}
			for pid, entry := range entries {
				name := windows.UTF16ToString(entry.ExeFile[:])
				for _, selector := range selectors {
					if !strings.EqualFold(name, filepath.Base(selector.Executable)) {
						continue
					}
					path, ok := paths[pid]
					if !ok {
						path.path, path.created, path.err = windowsProcessPath(pid)
						paths[pid] = path
					}
					if path.err != nil {
						svc.Complete = false
						svc.Issues["executable"] = "one or more matching process images could not be verified"
						continue
					}
					if strings.EqualFold(filepath.Clean(path.path), filepath.Clean(selector.Executable)) {
						roots = append(roots, pid)
						break
					}
				}
			}
			if len(roots) > 0 {
				svc.State = "running"
			} else if !svc.Complete {
				svc.State = "unknown"
			}
		}

		slices.Sort(roots)
		seen := map[uint32]bool{}
		queue := slices.Clone(roots)
		var threads, rss uint64
		memoryComplete := true
		for i := 0; i < len(queue); i++ {
			pid := queue[i]
			if seen[pid] {
				continue
			}
			seen[pid] = true
			entry, ok := entries[pid]
			if !ok {
				svc.Complete = false
				svc.Issues["process"] = "service PID absent from process snapshot"
				continue
			}
			if !attempted[pid] && len(attempted) < cfg.processLimit {
				attempted[pid] = true
				proc, counters, err := readWindowsProcess(entry, snapshot.CollectedAt)
				if err == nil {
					measured[pid] = &proc
					raw.counters[proc.Key] = counters
				}
			}
			p := measured[pid]
			if p == nil {
				svc.Complete = false
				svc.Issues["process"] = "process limit reached or process could not be read"
				out.Truncated = out.Truncated || !attempted[pid]
				continue
			}
			if !slices.Contains(roots, pid) {
				parent := measured[entry.ParentProcessID]
				if parent == nil || parent.StartTicks > p.StartTicks {
					// The recorded parent PID may have been reused since the child started.
					svc.Complete = false
					continue
				}
			} else {
				if scmName == "" && paths[pid].created != p.StartTicks {
					svc.Complete = false
					svc.Issues["process_identity"] = "PID changed during executable matching"
					continue
				}
				if scmName != "" {
					status, err := queryWindowsService(scm, scmName)
					if err != nil || status.CurrentState != windows.SERVICE_RUNNING || status.ProcessId != pid {
						svc.Complete = false
						svc.State = "unknown"
						svc.Issues["process_identity"] = "SCM membership changed during sampling"
						continue
					}
				}
				svc.RootKeys = append(svc.RootKeys, p.Key)
			}
			p.ServiceKeys = append(p.ServiceKeys, key)
			svc.ProcessKeys = append(svc.ProcessKeys, p.Key)
			threads += p.Threads
			rss += p.RSSBytes
			memoryComplete = memoryComplete && p.WorkingSetBytes != nil
			if target.children() {
				queue = append(queue, children[pid]...)
			}
		}
		svc.RootCount = len(svc.RootKeys)
		svc.ProcessCount = len(seen)
		svc.ReportedCount = len(svc.ProcessKeys)
		svc.lifecycleRoots = svc.RootKeys
		sort.Strings(svc.ProcessKeys)
		sort.Strings(svc.RootKeys)
		if svc.Complete {
			svc.Threads = &threads
			if memoryComplete {
				svc.RSSSumBytes = &rss
			}
		}
		tracker := c.trackers[key]
		if tracker == nil {
			tracker = &serviceLifecycle{}
			c.trackers[key] = tracker
		}
		updateServiceLifecycle(tracker, &svc)
		if svc.ProcessCount > 0 {
			raw.services[key] = rawService{members: svc.ProcessKeys, complete: svc.Complete, sources: svc.Sources}
		}
		out.Matches[svc.Name] = svc.ProcessCount
		out.Services = append(out.Services, svc)
	}
	for _, p := range measured {
		if len(p.ServiceKeys) == 0 {
			continue
		}
		if parent := measured[uint32(p.PPID)]; parent != nil && len(parent.ServiceKeys) > 0 && parent.StartTicks <= p.StartTicks && parent.PID != p.PID {
			p.ParentKey = parent.Key
		}
		out.Items = append(out.Items, *p)
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].PID < out.Items[j].PID })
	out.Matched, out.Reported = len(attempted), len(out.Items)
}
