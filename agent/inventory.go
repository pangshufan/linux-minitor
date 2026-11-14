//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (a *agent) collectInventory(parent context.Context, kernel string) {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	s := &inventorySnapshot{
		HostID: a.hostID, CollectedAt: time.Now(), Issues: make(map[string]string),
		System:   map[string]string{"kernel_release": kernel, "architecture": runtime.GOARCH},
		Hardware: make(map[string]any), LVMStatus: "unavailable",
	}
	if hostname, err := os.Hostname(); err == nil {
		s.System["hostname"] = hostname
	}
	if bootID := firstText("/proc/sys/kernel/random/boot_id"); bootID != "" {
		s.System["boot_id"] = bootID
	}
	readSource(s.Issues, "/proc/stat", func(f []string) {
		if f[0] == "btime" {
			if n, ok := uintAt(f, 1); ok {
				s.System["booted_at"] = time.Unix(int64(n), 0).UTC().Format(time.RFC3339)
			}
		}
	})
	for _, line := range strings.Split(firstText("/etc/os-release", "/usr/lib/os-release"), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "ID", "VERSION_ID", "PRETTY_NAME":
			s.System[strings.ToLower(key)] = strings.Trim(value, "\"'")
		}
	}
	s.collectHardware()
	s.collectBlockDevices(ctx)
	s.collectLVM(ctx)
	s.collectDIMMs(ctx)
	if parent.Err() == nil {
		a.inventory.Store(s)
	}
}

func (s *inventorySnapshot) collectHardware() {
	dmi := make(map[string]string)
	for _, field := range []string{
		"sys_vendor", "product_name", "product_version", "product_serial", "product_uuid",
		"board_vendor", "board_name", "bios_vendor", "bios_version", "bios_date",
	} {
		if value := firstText("/sys/class/dmi/id/" + field); value != "" {
			dmi[field] = value
		}
	}
	if len(dmi) > 0 {
		s.Hardware["dmi"] = dmi
	}
	if total, ok := readMemory(s.Issues)["total"]; ok {
		s.Hardware["os_memory_total_bytes"] = total
	}
	cpu := map[string]any{"online_logical_cpus": runtime.NumCPU()}
	if entries, err := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*"); err == nil && len(entries) > 0 {
		cpu["logical_cpus_visible"] = len(entries)
		packages := make(map[string]bool)
		cores := make(map[string]bool)
		complete := true
		for _, path := range entries {
			pkg := firstText(path + "/topology/physical_package_id")
			core := firstText(path + "/topology/core_id")
			if pkg == "" || pkg == "-1" || core == "" || core == "-1" {
				complete = false
				continue
			}
			packages[pkg], cores[pkg+":"+core] = true, true
		}
		if complete {
			cpu["packages_visible"] = len(packages)
			cpu["cores_visible"] = len(cores)
		}
	}
	if online := firstText("/sys/devices/system/cpu/online"); online != "" {
		cpu["online_cpu_list"] = online
	}
	for _, line := range strings.Split(firstText("/proc/cpuinfo"), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && (strings.TrimSpace(key) == "model name" || strings.TrimSpace(key) == "Hardware") {
			cpu["model"] = strings.TrimSpace(value)
			break
		}
	}
	s.Hardware["cpu"] = cpu

	if interfaces, err := net.Interfaces(); err != nil {
		s.Issues["network_inventory"] = err.Error()
	} else {
		items := make([]map[string]any, 0, len(interfaces))
		for _, nic := range interfaces {
			row := map[string]any{"name": nic.Name, "ifindex": nic.Index, "mac": nic.HardwareAddr.String(), "mtu": nic.MTU}
			path := filepath.Join("/sys/class/net", nic.Name)
			if state := firstText(path + "/operstate"); state != "" {
				row["oper_state"] = state
			}
			if speed := fileUint(path + "/speed"); speed > 0 {
				row["speed_mbps"] = speed
			}
			if driver, err := os.Readlink(path + "/device/driver"); err == nil {
				row["driver"] = filepath.Base(driver)
			}
			items = append(items, row)
		}
		s.Hardware["network_interfaces"] = items
	}
}

func (s *inventorySnapshot) collectDIMMs(ctx context.Context) {
	data, err := commandOutput(ctx, "dmidecode", "--type", "17")
	if err != nil {
		s.Issues["memory_devices"] = err.Error()
		return
	}
	devices := make([]map[string]string, 0)
	var device map[string]string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "Memory Device" {
			device = make(map[string]string)
			devices = append(devices, device)
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if device == nil || !ok {
			continue
		}
		switch key {
		case "Locator", "Bank Locator", "Size", "Type", "Speed", "Configured Memory Speed", "Manufacturer", "Part Number":
			device[strings.ReplaceAll(strings.ToLower(key), " ", "_")] = strings.TrimSpace(value)
		}
	}
	if len(devices) > 0 {
		s.Hardware["memory_devices"] = devices
	} else {
		s.Issues["memory_devices"] = "no SMBIOS memory device records"
	}
}

func (s *inventorySnapshot) collectBlockDevices(ctx context.Context) {
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		s.Issues["block_devices"] = err.Error()
		return
	}
	s.BlockDevices = make(map[string]*blockDevice, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join("/sys/class/block", name)
		id := firstText(path + "/dev")
		if id == "" {
			continue
		}
		d := &blockDevice{
			Name: name, Path: "/dev/" + name, Kind: "disk", SizeBytes: 512 * fileUint(path+"/size"),
			Model: firstText(path + "/device/model"), Serial: firstText(path+"/device/serial", path+"/serial"),
			LogicalSectorBytes:  fileUint(path + "/queue/logical_block_size"),
			PhysicalSectorBytes: fileUint(path + "/queue/physical_block_size"),
		}
		if part := fileUint(path + "/partition"); part > 0 {
			d.Kind, d.PartitionNumber = "part", part
			if start, err := strconv.ParseUint(firstText(path+"/start"), 10, 64); err == nil {
				d.StartSector512 = &start
			}
			if real, err := filepath.EvalSymlinks(path); err == nil {
				if parent := firstText(filepath.Dir(real) + "/dev"); parent != "" {
					d.Parents = append(d.Parents, parent)
				}
			}
		}
		if name := firstText(path + "/dm/name"); name != "" {
			d.Path, d.Kind = "/dev/mapper/"+name, "dm"
		}
		if slaves, err := os.ReadDir(path + "/slaves"); err == nil {
			for _, slave := range slaves {
				if parent := firstText(filepath.Join(path, "slaves", slave.Name(), "dev")); parent != "" {
					d.Parents = append(d.Parents, parent)
				}
			}
		}
		s.BlockDevices[id] = d
	}

	columns := "NAME,MAJ:MIN,TYPE,MODEL,SERIAL,WWN,FSTYPE,UUID,LABEL,PTTYPE,PARTTYPE,PARTUUID"
	data, err := commandOutput(ctx, "lsblk", "--json", "--list", "--output", columns)
	if err != nil {
		data, err = commandOutput(ctx, "lsblk", "--json", "--list", "--output", "NAME,MAJ:MIN,TYPE,FSTYPE,UUID,LABEL,PTTYPE")
	}
	var metadata struct {
		Devices []map[string]string `json:"blockdevices"`
	}
	if err == nil {
		err = json.Unmarshal(data, &metadata)
	}
	if err != nil {
		s.Issues["lsblk"] = err.Error()
	}
	for _, row := range metadata.Devices {
		d := s.BlockDevices[row["maj:min"]]
		if d == nil {
			continue
		}
		if kind := row["type"]; kind != "" {
			d.Kind = kind
		}
		d.Model = firstNonempty(strings.TrimSpace(row["model"]), d.Model)
		d.Serial = firstNonempty(strings.TrimSpace(row["serial"]), d.Serial)
		d.WWN = row["wwn"]
		d.Signature, d.SignatureUUID, d.Label = row["fstype"], row["uuid"], row["label"]
		d.PartitionType, d.PartitionUUID = row["parttype"], row["partuuid"]
		if d.Kind != "part" {
			d.PartitionTable = row["pttype"]
		}
	}
	for _, d := range s.BlockDevices {
		if ctx.Err() != nil {
			s.Issues["partition_space"] = ctx.Err().Error()
			break
		}
		if d.Kind == "disk" && d.PartitionTable != "" && d.LogicalSectorBytes > 0 {
			space, err := readPartitionSpace(ctx, d)
			if err != nil {
				s.Issues["partition_space/"+d.Name] = err.Error()
			} else {
				d.Unpartitioned = space
			}
		}
	}
}

func readPartitionSpace(ctx context.Context, d *blockDevice) (*partitionSpace, error) {
	data, err := commandOutput(ctx, "sfdisk", "--list-free", "--output", "Start,End,Sectors", d.Path)
	if err != nil {
		return nil, err
	}
	return parsePartitionSpace(string(data), d.LogicalSectorBytes)
}

func parsePartitionSpace(data string, sectorBytes uint64) (*partitionSpace, error) {
	space := &partitionSpace{SectorBytes: sectorBytes, Regions: []freeRegion{}}
	hasHeader := false
	zeroSectors := false
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, "Unpartitioned space ") && strings.HasSuffix(line, ", 0 sectors") {
			zeroSectors = true
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		if strings.EqualFold(fields[0], "Start") && strings.EqualFold(fields[1], "End") && strings.EqualFold(fields[2], "Sectors") {
			hasHeader = true
			continue
		}
		if !hasHeader {
			continue
		}
		values, ok := uints(fields)
		if !ok {
			return nil, fmt.Errorf("unrecognized sfdisk free range %q", line)
		}
		space.Regions = append(space.Regions, freeRegion{StartSector: values[0], EndSector: values[1], Sectors: values[2]})
		space.Bytes += values[2] * sectorBytes
	}
	if !hasHeader && !zeroSectors {
		return nil, fmt.Errorf("unrecognized sfdisk free-space report")
	}
	return space, nil
}

func (s *inventorySnapshot) collectLVM(ctx context.Context) {
	pv := "pv_name,pv_uuid,pv_size,pv_free,vg_name,vg_uuid"
	vg := "vg_name,vg_uuid,vg_size,vg_free,pv_count,lv_count"
	lv := "lv_name,lv_uuid,lv_path,vg_name,vg_uuid,lv_size,lv_attr,data_percent,metadata_percent"
	seg := "lv_uuid,segtype,seg_start,seg_size,devices"
	inventory := &lvmInventory{}
	data, err := commandOutput(ctx, "lvm", "fullreport", "--reportformat", "json", "--units", "b", "--nosuffix",
		"--configreport", "pv", "-o", pv,
		"--configreport", "vg", "-o", vg,
		"--configreport", "lv", "-o", lv,
		"--configreport", "seg", "-o", seg)
	if err == nil {
		reports, err := parseLVMReports(data)
		if err == nil {
			inventory.PVs, inventory.VGs, inventory.LVs, inventory.Segments = reports["pv"], reports["vg"], reports["lv"], reports["seg"]
			s.LVM, s.LVMStatus = inventory, "ok"
			return
		}
	}

	successes := 0
	for _, part := range []struct {
		cmd, name, fields string
		args              []string
		target            *[]map[string]string
	}{
		{"pvs", "pv", pv, nil, &inventory.PVs},
		{"vgs", "vg", vg, nil, &inventory.VGs},
		{"lvs", "lv", lv, nil, &inventory.LVs},
		{"lvs", "seg", seg, []string{"--segments"}, &inventory.Segments},
	} {
		rows, err := readLVMRows(ctx, part.cmd, part.name, part.fields, part.args)
		if err != nil {
			s.Issues["lvm/"+part.name] = err.Error()
			continue
		}
		*part.target = rows
		successes++
	}
	if successes > 0 {
		s.LVM, s.LVMStatus = inventory, "partial"
		if successes == 4 {
			s.LVMStatus = "ok"
		}
	}
}

func parseLVMReports(data []byte) (map[string][]map[string]string, error) {
	var result struct {
		Report []map[string][]map[string]string `json:"report"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.Report == nil {
		return nil, fmt.Errorf("LVM response is missing report")
	}
	combined := map[string][]map[string]string{"pv": {}, "vg": {}, "lv": {}, "seg": {}}
	for _, report := range result.Report {
		for key := range combined {
			combined[key] = append(combined[key], report[key]...)
		}
	}
	return combined, nil
}

func readLVMRows(ctx context.Context, cmd, report, fields string, opts []string) ([]map[string]string, error) {
	args := append([]string{"--units", "b", "--nosuffix", "-o", fields}, opts...)
	data, err := commandOutput(ctx, cmd, append(args, "--reportformat", "json")...)
	if err == nil {
		if reports, err := parseLVMReports(data); err == nil {
			return reports[report], nil
		}
	}
	data, err = commandOutput(ctx, cmd, append(args, "--noheadings", "--separator", "\t")...)
	if err != nil {
		return nil, err
	}
	keys := strings.Split(fields, ",")
	rows := make([]map[string]string, 0)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		values := strings.Split(line, "\t")
		if len(values) != len(keys) {
			return nil, fmt.Errorf("unexpected %s column count", cmd)
		}
		row := make(map[string]string, len(keys))
		for i, key := range keys {
			row[key] = strings.TrimSpace(values[i])
		}
		rows = append(rows, row)
	}
	return rows, nil
}

var mountUnescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, "\\")

func (a *agent) collectFilesystems() {
	s := &filesystemSnapshot{CollectedAt: time.Now(), Mounts: []mountPoint{}}
	err := readFields("/proc/self/mountinfo", func(f []string) {
		if len(f) < 10 {
			return
		}
		for i := 6; i+3 < len(f); i++ {
			if f[i] != "-" {
				continue
			}
			m := mountPoint{
				ID: f[0], Device: f[2], Root: mountUnescape.Replace(f[3]),
				Target: mountUnescape.Replace(f[4]), FSType: f[i+1], Source: mountUnescape.Replace(f[i+2]),
				ReadOnly: strings.HasPrefix(f[5]+",", "ro,") || strings.HasPrefix(f[i+3]+",", "ro,"),
			}
			s.Mounts = append(s.Mounts, m)
			break
		}
	})
	if err != nil {
		s.Issue = err.Error()
	}
	for i := range s.Mounts {
		m := &s.Mounts[i]
		if !localCapacityType(m.FSType) {
			m.CapacityIssue = "capacity sampling skipped for remote, virtual or unrecognized filesystem"
			continue
		}
		capacity, err := filesystemUsage(m.Target)
		if err != nil {
			m.CapacityIssue = err.Error()
			continue
		}
		at := time.Now()
		m.Capacity, m.CapacityAt = &capacity, &at
	}
	a.filesystems.Store(s)
}

func localCapacityType(kind string) bool {
	switch kind {
	case "ext2", "ext3", "ext4", "xfs", "btrfs", "vfat", "exfat", "f2fs", "zfs", "jfs", "reiserfs",
		"ntfs", "ntfs3", "iso9660", "udf", "squashfs", "tmpfs", "overlay", "erofs", "bcachefs":
		return true
	default:
		return false
	}
}

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	data, err := cmd.Output()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return data, nil
}

func firstText(paths ...string) string {
	for _, path := range paths {
		if data, err := os.ReadFile(path); err == nil {
			if value := strings.TrimSpace(string(data)); value != "" {
				return value
			}
		}
	}
	return ""
}

func fileUint(path string) uint64 {
	n, _ := strconv.ParseUint(firstText(path), 10, 64)
	return n
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
