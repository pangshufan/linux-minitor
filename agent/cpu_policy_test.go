package main

import (
	"math"
	"testing"
)

func TestWindowsCPUVersionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name            string
		build, revision uint32
		known, server   bool
		mode, want      string
	}{
		{"server2019", 17763, 8000, true, true, "auto", "utility"},
		{"server2022", 20348, 5000, true, true, "auto", "utility"},
		{"server2025before", 26100, 4651, true, true, "auto", "utility"},
		{"server2025standard", 26100, 4652, true, true, "auto", "time"},
		{"win10", 19045, 8000, true, false, "auto", "utility"},
		{"win11old", 22631, 7000, true, false, "auto", "utility"},
		{"win11before", 26100, 3623, true, false, "auto", "utility"},
		{"win11rollout", 26100, 3624, true, false, "auto", "time"},
		{"win11modern", 26200, 0, true, false, "auto", "time"},
		{"unknownpatch", 26100, 0, false, true, "auto", "time"},
		{"overrideutility", 26200, 9000, true, false, "utility", "utility"},
		{"overridetime", 17763, 8000, true, true, "time", "time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selection := windowsCPUPreference(tc.mode, tc.build, tc.revision, tc.known, tc.server)
			if selection.Preferred != tc.want || selection.Reason == "" || selection.WindowsBuild == "" {
				t.Fatalf("policy: %+v", selection)
			}
		})
	}
}

func TestWindowsCPUSelectionAndFallback(t *testing.T) {
	timeValue, utility, kernel := 35.0, 135.0, 34.0
	selection, values := chooseWindowsCPU(cpuSelection{Preferred: "utility"}, &timeValue, &utility, &kernel)
	if selection.Actual != "utility" || selection.Fallback != "" || values["busy"] != 100 || values["utility"] != 135 || values["time_busy"] != 35 {
		t.Fatalf("utility selection: %+v %v", selection, values)
	}
	selection, values = chooseWindowsCPU(cpuSelection{Preferred: "utility"}, &timeValue, nil, &kernel)
	if selection.Actual != "time" || selection.Source != "pdh_processor_time" || selection.Fallback == "" || values["busy"] != 35 {
		t.Fatalf("PDH fallback: %+v %v", selection, values)
	}
	selection, values = chooseWindowsCPU(cpuSelection{Preferred: "time"}, nil, &utility, &kernel)
	if selection.Source != "GetSystemTimes" || selection.Fallback == "" || values["busy"] != 34 {
		t.Fatalf("kernel fallback: %+v %v", selection, values)
	}
	nan := math.NaN()
	selection, values = chooseWindowsCPU(cpuSelection{Preferred: "utility"}, nil, &nan, nil)
	if _, ok := values["busy"]; ok || selection.Actual != "" {
		t.Fatalf("missing/invalid counters must not generate a CPU percentage: %+v %v", selection, values)
	}
}
