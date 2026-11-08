//go:build !linux

package main

func (c *processCollector) pollProcessExits() bool              { c.pidfdUnavailable = true; return false }
func (c *processCollector) trackProcessHandles([]processMetric) {}
func (c *processCollector) close()                              {}
