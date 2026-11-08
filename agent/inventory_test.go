package main

import "testing"

func TestPartitionSpaceReport(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		bytes        uint64
		regions      int
	}{
		{"no free sectors on UOS", "Unpartitioned space /dev/vda: 0 B, 0 bytes, 0 sectors\nUnits: sectors of 1 * 512 = 512 bytes\n", 0, 0},
		{"multiple ranges", "Start End Sectors\n2048 2057 10\n4096 4115 20\n", 30 * 512, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			space, err := parsePartitionSpace(tc.output, 512)
			if err != nil || space.Bytes != tc.bytes || len(space.Regions) != tc.regions {
				t.Fatalf("unexpected free-space report: %+v, %v", space, err)
			}
		})
	}
	if _, err := parsePartitionSpace("Unpartitioned space /dev/vda: 5 KiB, 5120 bytes, 10 sectors", 512); err == nil {
		t.Fatal("missing range table should be rejected when sectors are free")
	}
}
