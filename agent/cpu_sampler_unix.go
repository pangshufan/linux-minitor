//go:build !windows

package main

type platformCPUSampler struct{}

func newPlatformCPUSampler(config) *platformCPUSampler                      { return &platformCPUSampler{} }
func (*platformCPUSampler) collect(*rawSample)                              {}
func (*platformCPUSampler) close()                                          {}
func calculatePlatformCPU(*metricWindow, rawSample, []rawSample, rawSample) {}
