package main

import (
	"net"
	"sort"
)

// Addresses retain CIDR prefixes so the UI can distinguish subnet configuration.
func interfaceAddresses(nic net.Interface) ([]string, error) {
	addrs, err := nic.Addrs()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		result = append(result, addr.String())
	}
	sort.Strings(result)
	return result, nil
}

func networkAddressInventory() (map[string][]string, map[string]string) {
	addresses := make(map[string][]string)
	issues := make(map[string]string)
	interfaces, err := net.Interfaces()
	if err != nil {
		issues["network_addresses"] = err.Error()
		return addresses, issues
	}
	for _, nic := range interfaces {
		items, err := interfaceAddresses(nic)
		if err != nil {
			issues["network_addresses/"+nic.Name] = err.Error()
			continue
		}
		addresses[nic.Name] = items
	}
	return addresses, issues
}
