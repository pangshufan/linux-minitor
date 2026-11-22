package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestHostSummaryAddresses(t *testing.T) {
	// Private, public and IPv6 addresses are configuration, not peer/NAT addresses.
	inventory := json.RawMessage(`{"system":{"hostname":"node"},"hardware":{"network_interfaces":[
		{"name":"eth0","addresses":["10.181.21.20/24","2001:db8::20/64","fe80::1/64"]},
		{"name":"eth1","addresses":["10.181.21.20/16","192.168.1.2/24","::ffff:10.181.21.20","invalid"]},
		{"name":"lo","addresses":["127.0.0.1/8","::1/128","0.0.0.0","::","ff02::1"]},
		{"name":"legacy"}
	]}}`)
	got := summarize(hostRecord{ID: "host-20", Inventory: inventory})
	want := []string{"10.181.21.20", "192.168.1.2", "2001:db8::20"}
	if got.ID != "host-20" || !reflect.DeepEqual(got.Addresses, want) {
		t.Fatalf("got %+v, want addresses %v", got, want)
	}
	legacy := summarize(hostRecord{ID: "legacy", Inventory: json.RawMessage(`{"system":{"hostname":"old-agent"}}`)})
	if len(legacy.Addresses) != 0 || legacy.Name != "old-agent" {
		t.Fatalf("legacy inventory compatibility failed: %+v", legacy)
	}
}
