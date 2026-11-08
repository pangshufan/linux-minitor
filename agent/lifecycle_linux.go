package main

import (
	"errors"
	"golang.org/x/sys/unix"
)

func (c *processCollector) pollProcessExits() bool {
	polls := []unix.PollFd{}
	keys := []string{}
	for key, fd := range c.pidfds {
		polls = append(polls, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
		keys = append(keys, key)
	}
	if len(polls) == 0 {
		return false
	}
	if _, err := unix.Poll(polls, 0); err != nil {
		return false
	}
	exited := false
	for i, p := range polls {
		if p.Revents != 0 {
			unix.Close(int(p.Fd))
			delete(c.pidfds, keys[i])
			exited = true
		}
	}
	return exited
}
func (c *processCollector) trackProcessHandles(items []processMetric) {
	if c.root != "/proc" {
		c.pidfdUnavailable = true
		return
	}
	if c.pidfds == nil {
		c.pidfds = map[string]int{}
	}
	alive := map[string]bool{}
	for _, p := range items {
		alive[p.Key] = true
		if _, ok := c.pidfds[p.Key]; ok || c.pidfdUnavailable {
			continue
		}
		fd, err := unix.PidfdOpen(p.PID, 0)
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			c.pidfdUnavailable = true
			continue
		}
		if err != nil {
			continue
		}
		check, err := c.stat(p.PID)
		if err != nil || check.Key != p.Key {
			unix.Close(fd)
			continue
		}
		c.pidfds[p.Key] = fd
	}
	for key, fd := range c.pidfds {
		if !alive[key] {
			unix.Close(fd)
			delete(c.pidfds, key)
		}
	}
}
func (c *processCollector) close() {
	for _, fd := range c.pidfds {
		unix.Close(fd)
	}
	c.pidfds = nil
}
