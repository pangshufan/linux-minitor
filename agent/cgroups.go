package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type cgroupMount struct {
	path, root  string
	v2          bool
	controllers map[string]bool
}

func readCgroupMounts(path string) ([]cgroupMount, error) {
	mounts := []cgroupMount{}
	err := readFields(path, func(f []string) {
		for i := 6; i+3 < len(f); i++ {
			if f[i] != "-" {
				continue
			}
			if f[i+1] != "cgroup" && f[i+1] != "cgroup2" {
				return
			}
			m := cgroupMount{path: mountUnescape.Replace(f[4]), root: mountUnescape.Replace(f[3]), v2: f[i+1] == "cgroup2", controllers: map[string]bool{}}
			for _, name := range strings.Split(f[i+3], ",") {
				m.controllers[name] = true
			}
			mounts = append(mounts, m)
			return
		}
	})
	// Prefer unified cgroup, then systemd membership on v1.
	sort.SliceStable(mounts, func(i, j int) bool {
		if mounts[i].v2 != mounts[j].v2 {
			return mounts[i].v2
		}
		return mounts[i].controllers["name=systemd"] && !mounts[j].controllers["name=systemd"]
	})
	return mounts, err
}
func (m cgroupMount) directory(group string) (string, bool) {
	if !filepath.IsAbs(group) || group == "/" {
		return "", false
	}
	rel, err := filepath.Rel(m.root, group)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(m.path, rel), true
}
func (c *processCollector) groupPIDs(group string, recursive bool) ([]int, error) {
	var last error
	for _, m := range c.mounts {
		dir, ok := m.directory(group)
		if !ok {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			last = err
			continue
		}
		out, partial := readGroupPIDs(dir, recursive)
		if c.groupMemberships == nil {
			c.groupMemberships = map[string]groupMembership{}
		}
		c.groupMemberships[group] = groupMembership{mount: m, pids: out}
		return out, partial

	}
	if last == nil {
		last = fmt.Errorf("no readable cgroup hierarchy for %s", group)
	}
	return nil, last
}

type groupMembership struct {
	mount cgroupMount
	pids  []int
}

func readGroupPIDs(dir string, recursive bool) ([]int, error) {
	found := map[int]bool{}
	queue := []string{dir}
	var partial error
	for i := 0; i < len(queue); i++ {
		if len(queue) > 10000 {
			partial = fmt.Errorf("cgroup subtree exceeds 10000 directories")
			break
		}
		data, err := os.ReadFile(filepath.Join(queue[i], "cgroup.procs"))
		if err != nil {
			partial = err
		} else {
			for _, value := range strings.Fields(string(data)) {
				pid, err := strconv.Atoi(value)
				if err != nil || pid <= 0 {
					partial = fmt.Errorf("invalid cgroup PID")
					continue
				}
				found[pid] = true
			}
		}
		if !recursive {
			continue
		}
		entries, err := os.ReadDir(queue[i])
		if err != nil {
			partial = err
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				queue = append(queue, filepath.Join(queue[i], entry.Name()))
			}
		}
	}
	out := []int{}
	for pid := range found {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out, partial
}
func groupIdentity(path string) string {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err == nil {
		return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	return path
}
func (c *processCollector) groupMetrics(group string) (map[string]uint64, *uint64, map[string]string, string, map[string]string) {
	counters := map[string]uint64{}
	sources := map[string]string{}
	issues := map[string]string{}
	identityParts := []string{}
	var memory *uint64
	for _, m := range c.mounts {
		dir, ok := m.directory(group)
		if !ok {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if selected, ok := c.groupMemberships[group]; ok {
			if selected.mount.v2 != m.v2 || (m.v2 && selected.mount.path != m.path) {
				continue
			}
			if !m.v2 && selected.mount.path != m.path {
				pids, err := readGroupPIDs(dir, true)
				equal := err == nil && len(pids) == len(selected.pids)
				if equal {
					for i, pid := range pids {
						if pid != selected.pids[i] {
							equal = false
							break
						}
					}
				}
				if !equal {
					issues["controller_membership/"+m.path] = "controller group membership differs from selected service hierarchy"
					continue
				}
			}
		}
		source := "cgroup_v1"
		if m.v2 {
			source = "cgroup_v2"
		}
		readNumber := func(file string) (uint64, bool) {
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				if !os.IsNotExist(err) {
					issues[file] = err.Error()
				}
				return 0, false
			}
			n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				issues[file] = "invalid cgroup counter"
				return 0, false
			}
			return n, true
		}
		if m.v2 {
			if data, err := os.ReadFile(filepath.Join(dir, "cpu.stat")); err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					f := strings.Fields(line)
					if len(f) == 2 && f[0] == "usage_usec" {
						n, err := strconv.ParseUint(f[1], 10, 64)
						if err == nil {
							counters["cpu_time_ns"] = n * 1000
							sources["cpu"] = source
							identityParts = append(identityParts, "cpu:"+groupIdentity(dir))
						}
					}
				}
			} else if !os.IsNotExist(err) {
				issues["cpu.stat"] = err.Error()
			}
			if n, ok := readNumber("memory.current"); ok {
				memory = &n
				sources["memory"] = source
			}
			if data, err := os.ReadFile(filepath.Join(dir, "io.stat")); err == nil {
				var reads, writes uint64
				hasRead, hasWrite := false, false
				for _, line := range strings.Split(string(data), "\n") {
					f := strings.Fields(line)
					if len(f) == 0 {
						continue
					}
					for _, field := range f[1:] {
						key, text, ok := strings.Cut(field, "=")
						if !ok {
							continue
						}
						n, err := strconv.ParseUint(text, 10, 64)
						if err != nil {
							continue
						}
						switch key {
						case "rbytes":
							reads += n
							hasRead = true
						case "wbytes":
							writes += n
							hasWrite = true
						}
					}
				}
				// An empty readable io.stat means the cgroup has no accounted device I/O yet.
				if len(strings.TrimSpace(string(data))) == 0 {
					hasRead, hasWrite = true, true
				}
				if hasRead {
					counters["read_bytes"] = reads
				}
				if hasWrite {
					counters["write_bytes"] = writes
				}
				if hasRead && hasWrite {
					sources["io"] = source
					identityParts = append(identityParts, "io:"+groupIdentity(dir))
				}
			} else if !os.IsNotExist(err) {
				issues["io.stat"] = err.Error()
			}
			// Do not borrow controller counters from a different membership hierarchy.
		} else {
			if sources["cpu"] == "" && m.controllers["cpuacct"] {
				if n, ok := readNumber("cpuacct.usage"); ok {
					counters["cpu_time_ns"] = n
					sources["cpu"] = source
					identityParts = append(identityParts, "cpu:"+groupIdentity(dir))
				}
			}
			if memory == nil && m.controllers["memory"] {
				if n, ok := readNumber("memory.usage_in_bytes"); ok {
					memory = &n
					sources["memory"] = source
				}
			}
			if sources["io"] == "" && m.controllers["blkio"] {
				for _, file := range []string{"blkio.throttle.io_service_bytes_recursive", "blkio.io_service_bytes_recursive"} {
					data, err := os.ReadFile(filepath.Join(dir, file))
					if err != nil {
						if !os.IsNotExist(err) {
							issues[file] = err.Error()
						}
						continue
					}
					var reads, writes uint64
					valid := true
					for _, line := range strings.Split(string(data), "\n") {
						f := strings.Fields(line)
						if len(f) != 3 || !strings.Contains(f[0], ":") {
							continue
						}
						n, err := strconv.ParseUint(f[2], 10, 64)
						if err != nil {
							valid = false
							break
						}
						switch f[1] {
						case "Read":
							reads += n
						case "Write":
							writes += n
						}
					}
					if valid {
						counters["read_bytes"], counters["write_bytes"] = reads, writes
						sources["io"] = source
						identityParts = append(identityParts, "io:"+groupIdentity(dir))
					}
					break
				}
			}
		}
	}
	return counters, memory, sources, strings.Join(identityParts, ";"), issues
}

func (c *processCollector) groupToken(group string) string {
	for _, m := range c.mounts {
		dir, ok := m.directory(group)
		if !ok {
			continue
		}
		if _, err := os.Stat(dir); err == nil {
			return groupIdentity(dir)
		}
	}
	return ""
}
