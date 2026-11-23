package main

import (
	"fmt"
	"math"
)

type cpuSelection struct {
	CounterIssues map[string]string `json:"counter_issues,omitempty"`
	Requested     string            `json:"requested"`
	Preferred     string            `json:"preferred"`
	Actual        string            `json:"actual"`
	Source        string            `json:"source"`
	Reason        string            `json:"reason"`
	Fallback      string            `json:"fallback,omitempty"`
	WindowsBuild  string            `json:"windows_build"`
}

func validWindowsCPUMode(mode string) bool {
	return mode == "" || mode == "auto" || mode == "time" || mode == "utility"
}

// Patch thresholds are compatibility rules, not runtime detection of Task Manager.
// Gradual feature rollout can differ on identical builds; retain an explicit override.
func windowsCPUPreference(mode string, build, revision uint32, revisionKnown, server bool) cpuSelection {
	if mode == "" {
		mode = "auto"
	}
	result := cpuSelection{Requested: mode, Preferred: mode, WindowsBuild: fmt.Sprintf("10.0.%d", build)}
	if revisionKnown {
		result.WindowsBuild += fmt.Sprintf(".%d", revision)
	}
	if mode != "auto" {
		result.Reason = "explicit_configuration"
		return result
	}
	switch {
	case build >= 26200:
		result.Preferred = "time"
		result.Reason = "modern_windows_standard_metrics"
	case build == 26100 && revisionKnown && server && revision >= 4652:
		result.Preferred = "time"
		result.Reason = "server_2025_KB5062553_or_later"
	case build == 26100 && revisionKnown && !server && revision >= 3624:
		result.Preferred = "time"
		result.Reason = "windows_11_KB5053656_standard_metrics_rollout"
	case build == 26100 && !revisionKnown:
		result.Preferred = "time"
		result.Reason = "unknown_patch_level_conservative_time"
	default:
		result.Preferred = "utility"
		result.Reason = "legacy_windows_utility_default"
	}
	return result
}

// Values supplied by Windows remain independent. Never invent a frequency/core weighting.
func chooseWindowsCPU(selection cpuSelection, timeValue, utilityValue, kernelValue *float64) (cpuSelection, map[string]float64) {
	values := make(map[string]float64)
	valid := func(v *float64) bool { return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0 }
	if valid(timeValue) {
		values["time_busy"] = *timeValue
	}
	if valid(utilityValue) {
		values["utility"] = *utilityValue
	}
	if valid(kernelValue) {
		values["kernel_busy"] = *kernelValue
	}
	var selected *float64
	switch {
	case selection.Preferred == "utility" && valid(utilityValue):
		selected = utilityValue
		selection.Actual = "utility"
		selection.Source = "pdh_processor_utility"
	case valid(timeValue):
		selected = timeValue
		selection.Actual = "time"
		selection.Source = "pdh_processor_time"
	case valid(kernelValue):
		selected = kernelValue
		selection.Actual = "time"
		selection.Source = "GetSystemTimes"
	}
	if selection.Actual != selection.Preferred {
		selection.Fallback = "preferred_counter_unavailable"
	} else if selection.Source == "GetSystemTimes" {
		selection.Fallback = "pdh_time_counter_unavailable"
	}
	if selected != nil {
		values["busy"] = min(100, *selected)
	}
	return selection, values
}
