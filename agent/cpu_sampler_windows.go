package main

import (
	"fmt"
	"log"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	pdh           = windows.NewLazySystemDLL("pdh.dll")
	pdhOpenQuery  = pdh.NewProc("PdhOpenQueryW")
	pdhAddCounter = pdh.NewProc("PdhAddEnglishCounterW")
	pdhCollect    = pdh.NewProc("PdhCollectQueryData")
	pdhGetRaw     = pdh.NewProc("PdhGetRawCounterValue")
	pdhCalculate  = pdh.NewProc("PdhCalculateCounterFromRawValue")
	pdhCloseQuery = pdh.NewProc("PdhCloseQuery")
)

const (
	pdhTime    = 0
	pdhUtility = 1
)

var windowsCPUCounterPaths = [2]string{
	`\Processor Information(_Total)\% Processor Time`,
	`\Processor Information(_Total)\% Processor Utility`,
}

type pdhRawCounter struct {
	Status        uint32
	Timestamp     windows.Filetime
	First, Second int64
	MultiCount    uint32
}
type pdhFormatted struct {
	Status uint32
	Value  float64
}
type platformCPUSampler struct {
	query      uintptr
	handles    [2]uintptr
	selection  cpuSelection
	initIssues map[string]string
}
type windowsCPUSample struct {
	sampler *platformCPUSampler
	values  [2]pdhRawCounter
	valid   [2]bool
	issues  map[string]string
}

func detectedWindowsCPUPreference(mode string) cpuSelection {
	version := windows.RtlGetVersion()
	var revision uint32
	known := false
	if key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		if n, _, err := key.GetIntegerValue("UBR"); err == nil {
			revision = uint32(n)
			known = true
		}
		key.Close()
	}
	return windowsCPUPreference(mode, version.BuildNumber, revision, known, version.ProductType != 1)
}

func newPlatformCPUSampler(c config) *platformCPUSampler {
	result := &platformCPUSampler{selection: detectedWindowsCPUPreference(c.windowsCPUMode), initIssues: map[string]string{}}
	log.Printf("Windows CPU requested=%s preferred=%s build=%s reason=%s", result.selection.Requested, result.selection.Preferred, result.selection.WindowsBuild, result.selection.Reason)
	for _, proc := range []*windows.LazyProc{pdhOpenQuery, pdhAddCounter, pdhCollect, pdhGetRaw, pdhCalculate, pdhCloseQuery} {
		if err := proc.Find(); err != nil {
			result.initIssues["pdh"] = err.Error()
			return result
		}
	}
	status, _, _ := pdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&result.query)))
	if uint32(status) != 0 {
		result.initIssues["pdh"] = fmt.Sprintf("PdhOpenQuery status 0x%08x", uint32(status))
		return result
	}
	for i, path := range windowsCPUCounterPaths {
		u, err := windows.UTF16PtrFromString(path)
		if err != nil {
			result.initIssues[path] = err.Error()
			continue
		}
		status, _, _ := pdhAddCounter.Call(result.query, uintptr(unsafe.Pointer(u)), 0, uintptr(unsafe.Pointer(&result.handles[i])))
		if uint32(status) != 0 {
			result.handles[i] = 0
			result.initIssues[path] = fmt.Sprintf("PdhAddEnglishCounter status 0x%08x", uint32(status))
		}
	}
	return result
}

func (s *platformCPUSampler) close() {
	if s.query != 0 {
		pdhCloseQuery.Call(s.query)
		s.query = 0
	}
}

func (s *platformCPUSampler) collect(raw *rawSample) {
	sample := &windowsCPUSample{sampler: s, issues: map[string]string{}}
	for key, value := range s.initIssues {
		sample.issues[key] = value
	}
	raw.cpuNative = sample
	if s.query == 0 {
		return
	}
	status, _, _ := pdhCollect.Call(s.query)
	if uint32(status) != 0 {
		sample.issues["pdh"] = fmt.Sprintf("PdhCollectQueryData status 0x%08x", uint32(status))
		return
	}
	for i, handle := range s.handles {
		if handle == 0 {
			continue
		}
		status, _, _ := pdhGetRaw.Call(handle, 0, uintptr(unsafe.Pointer(&sample.values[i])))
		if uint32(status) != 0 || sample.values[i].Status > 1 {
			sample.issues[windowsCPUCounterPaths[i]] = fmt.Sprintf("PdhGetRawCounterValue status 0x%08x / counter 0x%08x", uint32(status), sample.values[i].Status)
			continue
		}
		sample.valid[i] = true
	}
}

func calculatePDHWindow(current, previous *windowsCPUSample, history []rawSample, base rawSample, index int) (*float64, error) {
	if previous == nil || current.sampler != previous.sampler || !current.valid[index] || !previous.valid[index] {
		return nil, fmt.Errorf("counter missing or query changed")
	}
	// Reject a window spanning a failed collection, counter reset, topology change,
	// or non-increasing counter timestamp. Do not average formatted one-second percentages.
	newer := current.values[index]
	for i := len(history) - 1; i >= 0 && !history[i].at.Before(base.at); i-- {
		sample, ok := history[i].cpuNative.(*windowsCPUSample)
		if !ok || sample.sampler != current.sampler || !sample.valid[index] {
			return nil, fmt.Errorf("counter unavailable inside window")
		}
		older := sample.values[index]
		if older.First > newer.First || older.Second > newer.Second || older.MultiCount != newer.MultiCount ||
			filetimeTicks(older.Timestamp) >= filetimeTicks(newer.Timestamp) {
			return nil, fmt.Errorf("counter reset, topology changed, or timestamp did not advance")
		}
		newer = older
	}
	var formatted pdhFormatted
	status, _, _ := pdhCalculate.Call(current.sampler.handles[index], 0x200|0x8000,
		uintptr(unsafe.Pointer(&current.values[index])), uintptr(unsafe.Pointer(&previous.values[index])), uintptr(unsafe.Pointer(&formatted)))
	if uint32(status) != 0 || formatted.Status > 1 || math.IsNaN(formatted.Value) || math.IsInf(formatted.Value, 0) || formatted.Value < 0 {
		return nil, fmt.Errorf("PdhCalculateCounterFromRawValue status 0x%08x / counter 0x%08x", uint32(status), formatted.Status)
	}
	return &formatted.Value, nil
}

func calculatePlatformCPU(w *metricWindow, current rawSample, history []rawSample, base rawSample) {
	native, ok := current.cpuNative.(*windowsCPUSample)
	if !ok {
		return
	}
	previous, _ := base.cpuNative.(*windowsCPUSample)
	var kernel *float64
	if value, ok := w.CPU["cpu"]["busy"]; ok {
		kernel = &value
	}
	timeValue, timeErr := calculatePDHWindow(native, previous, history, base, pdhTime)
	utilityValue, utilityErr := calculatePDHWindow(native, previous, history, base, pdhUtility)
	selection, values := chooseWindowsCPU(native.sampler.selection, timeValue, utilityValue, kernel)
	selection.CounterIssues = make(map[string]string)
	for key, value := range native.issues {
		selection.CounterIssues[key] = value
	}
	if timeErr != nil {
		selection.CounterIssues["time_window"] = timeErr.Error()
	}
	if utilityErr != nil {
		selection.CounterIssues["utility_window"] = utilityErr.Error()
	}
	w.CPUSelection = &selection
	delete(w.CPU["cpu"], "iowait")
	if w.CPU["cpu"] == nil {
		w.CPU["cpu"] = map[string]float64{}
	}
	for name, value := range values {
		w.CPU["cpu"][name] = value
	}
}
