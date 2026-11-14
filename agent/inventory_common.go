package main

import "time"

type inventorySnapshot struct {
	HostID       string                  `json:"host_id"`
	CollectedAt  time.Time               `json:"collected_at"`
	System       map[string]string       `json:"system"`
	Hardware     map[string]any          `json:"hardware"`
	BlockDevices map[string]*blockDevice `json:"block_devices"`
	LVM          *lvmInventory           `json:"lvm,omitempty"`
	LVMStatus    string                  `json:"lvm_status"`
	Issues       map[string]string       `json:"issues,omitempty"`
}

type blockDevice struct {
	Name                string          `json:"name"`
	Path                string          `json:"path"`
	Kind                string          `json:"kind"`
	SizeBytes           uint64          `json:"size_bytes"`
	Parents             []string        `json:"parents,omitempty"`
	Model               string          `json:"model,omitempty"`
	Serial              string          `json:"serial,omitempty"`
	WWN                 string          `json:"wwn,omitempty"`
	LogicalSectorBytes  uint64          `json:"logical_sector_bytes,omitempty"`
	PhysicalSectorBytes uint64          `json:"physical_sector_bytes,omitempty"`
	PartitionNumber     uint64          `json:"partition_number,omitempty"`
	StartSector512      *uint64         `json:"start_sector_512_bytes,omitempty"`
	PartitionTable      string          `json:"partition_table,omitempty"`
	PartitionType       string          `json:"partition_type,omitempty"`
	PartitionUUID       string          `json:"partition_uuid,omitempty"`
	Signature           string          `json:"content_type,omitempty"`
	SignatureUUID       string          `json:"content_uuid,omitempty"`
	Label               string          `json:"content_label,omitempty"`
	Unpartitioned       *partitionSpace `json:"unpartitioned,omitempty"`
}

type partitionSpace struct {
	SectorBytes uint64       `json:"sector_bytes"`
	Bytes       uint64       `json:"bytes"`
	Regions     []freeRegion `json:"regions"`
}

type freeRegion struct {
	StartSector uint64 `json:"start_sector"`
	EndSector   uint64 `json:"end_sector_inclusive"`
	Sectors     uint64 `json:"sectors"`
}

type lvmInventory struct {
	PVs      []map[string]string `json:"physical_volumes"`
	VGs      []map[string]string `json:"volume_groups"`
	LVs      []map[string]string `json:"logical_volumes"`
	Segments []map[string]string `json:"logical_volume_segments"`
}

type filesystemSnapshot struct {
	CollectedAt time.Time    `json:"collected_at"`
	Mounts      []mountPoint `json:"mounts"`
	Issue       string       `json:"issue,omitempty"`
}

type mountPoint struct {
	ID            string              `json:"mount_id"`
	Device        string              `json:"device"`
	Root          string              `json:"root"`
	Target        string              `json:"target"`
	FSType        string              `json:"fstype"`
	Source        string              `json:"source"`
	ReadOnly      bool                `json:"read_only"`
	Capacity      *filesystemCapacity `json:"capacity,omitempty"`
	CapacityAt    *time.Time          `json:"capacity_at,omitempty"`
	CapacityIssue string              `json:"capacity_issue,omitempty"`
}

type filesystemCapacity struct {
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	TotalInodes    uint64 `json:"total_inodes"`
	FreeInodes     uint64 `json:"free_inodes"`
}
