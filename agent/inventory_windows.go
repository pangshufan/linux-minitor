package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func (a *agent) collectInventory(ctx context.Context, build string) {
	host, _ := os.Hostname()
	s := &inventorySnapshot{HostID: a.hostID, CollectedAt: time.Now(), System: map[string]string{
		"hostname": host, "id": "windows", "kernel_release": build, "version_id": build, "architecture": runtime.GOARCH,
	}, Hardware: map[string]any{}, LVMStatus: "unsupported", Issues: map[string]string{}}
	s.System["booted_at"] = s.CollectedAt.Add(-windowsUptime()).UTC().Format(time.RFC3339)
	s.System["pretty_name"] = "Microsoft Windows"
	if key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		defer key.Close()
		if product, _, err := key.GetStringValue("ProductName"); err == nil && product != "" {
			s.System["pretty_name"] = product
		}
	}
	if dir, err := windows.GetSystemWindowsDirectory(); err == nil {
		u, err := windows.UTF16PtrFromString(dir)
		if err == nil {
			buf := make([]uint16, 32768)
			if err := windows.GetVolumePathName(u, &buf[0], uint32(len(buf))); err == nil {
				s.System["system_volume"] = windows.UTF16ToString(buf)
			}
		}
	}
	cpu := map[string]any{"logical_cpus_visible": windows.GetActiveProcessorCount(0xffff)}
	if key, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		defer key.Close()
		if name, _, err := key.GetStringValue("ProcessorNameString"); err == nil {
			cpu["model"] = strings.TrimSpace(name)
		}
	}
	s.Hardware["cpu"] = cpu
	if info, err := readWindowsMemory(); err == nil {
		s.Hardware["os_memory_total_bytes"] = info.TotalPhysical
	}
	var interfaces []map[string]any
	if err := windowsInterfaces(windows.MibIfTableNormal, func(r *windows.MibIfRow2) {
		entry := map[string]any{"name": windows.UTF16ToString(r.Alias[:]), "key": windowsInterfaceKey(r), "ifindex": r.InterfaceIndex,
			"oper_state": fmt.Sprint(r.OperStatus), "mtu": r.Mtu, "description": windows.UTF16ToString(r.Description[:])}
		if n := int(r.PhysicalAddressLength); n > 0 && n <= len(r.PhysicalAddress) {
			entry["mac"] = net.HardwareAddr(r.PhysicalAddress[:n]).String()
		}
		if r.TransmitLinkSpeed != 0 && r.TransmitLinkSpeed != ^uint64(0) {
			entry["speed_mbps"] = r.TransmitLinkSpeed / 1000000
		}
		interfaces = append(interfaces, entry)
	}); err != nil {
		s.Issues["network"] = err.Error()
	}
	if interfaces != nil {
		s.Hardware["network_interfaces"] = interfaces
	}
	if ctx.Err() == nil {
		a.inventory.Store(s)
	}
}

func windowsVolumePaths(volume *uint16) ([]string, error) {
	buf := make([]uint16, 512)
	var length uint32
	err := windows.GetVolumePathNamesForVolumeName(volume, &buf[0], uint32(len(buf)), &length)
	if errors.Is(err, windows.ERROR_MORE_DATA) && length > uint32(len(buf)) && length <= 1<<20 {
		buf = make([]uint16, length)
		err = windows.GetVolumePathNamesForVolumeName(volume, &buf[0], uint32(len(buf)), &length)
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for len(buf) > 0 && buf[0] != 0 {
		path := windows.UTF16ToString(buf)
		paths = append(paths, path)
		for len(buf) > 0 && buf[0] != 0 {
			buf = buf[1:]
		}
		if len(buf) > 0 {
			buf = buf[1:]
		}
	}
	return paths, nil
}

func (a *agent) collectFilesystems() {
	s := &filesystemSnapshot{CollectedAt: time.Now(), Mounts: []mountPoint{}}
	defer a.filesystems.Store(s)
	name := make([]uint16, 256)
	handle, err := windows.FindFirstVolume(&name[0], uint32(len(name)))
	if err != nil {
		s.Issue = err.Error()
		return
	}
	defer windows.FindVolumeClose(handle)
	for {
		if windows.GetDriveType(&name[0]) == windows.DRIVE_FIXED {
			paths, err := windowsVolumePaths(&name[0])
			if err != nil {
				s.Issue = err.Error()
			} else if len(paths) > 0 {
				volume := windows.UTF16ToString(name)
				for _, path := range paths {
					s.Mounts = append(s.Mounts, readWindowsVolume(volume, path, s.CollectedAt))
				}
			}
		}
		err = windows.FindNextVolume(handle, &name[0], uint32(len(name)))
		if err != nil {
			if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				s.Issue = err.Error()
			}
			return
		}
	}
}

func readWindowsVolume(volume, path string, at time.Time) mountPoint {
	m := mountPoint{ID: volume, Device: volume, Source: volume, Root: path, Target: path}
	root, err := windows.UTF16PtrFromString(path)
	if err != nil {
		m.CapacityIssue = err.Error()
		return m
	}
	var fs [256]uint16
	var flags uint32
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, &flags, &fs[0], uint32(len(fs))); err != nil {
		m.CapacityIssue = err.Error()
		return m
	}
	m.FSType = windows.UTF16ToString(fs[:])
	m.ReadOnly = flags&windows.FILE_READ_ONLY_VOLUME != 0
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(root, &available, &total, &free); err != nil {
		m.CapacityIssue = err.Error()
	} else if free > total || available > total {
		m.CapacityIssue = "volume capacity is inconsistent for the current account"
	} else {
		m.Capacity = &filesystemCapacity{TotalBytes: total, UsedBytes: total - free, AvailableBytes: available}
		m.CapacityAt = &at
	}
	return m
}
