package main

import (
	"net"
	"net/netip"
	"testing"
)

func TestNetworkAddressInventory(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("interface enumeration unavailable: %v", err)
	}
	addresses, issues := networkAddressInventory()
	for _, nic := range interfaces {
		expected, err := nic.Addrs()
		if err != nil {
			if issues["network_addresses/"+nic.Name] == "" {
				t.Fatalf("missing address collection issue for %s", nic.Name)
			}
			continue
		}
		actual, ok := addresses[nic.Name]
		if !ok || len(actual) != len(expected) {
			t.Fatalf("%s: %v, expected %v", nic.Name, actual, expected)
		}
		for _, address := range actual {
			if _, err := netip.ParsePrefix(address); err != nil {
				t.Fatalf("%s has invalid CIDR address %q: %v", nic.Name, address, err)
			}
		}
	}
}
