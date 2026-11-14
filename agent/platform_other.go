//go:build !linux && !windows

package main

import "errors"

func kernelRelease() (string, error) {
	return "", errors.New("Linux kernel unavailable")
}

func filesystemUsage(string) (filesystemCapacity, error) {
	return filesystemCapacity{}, errors.New("Linux statfs unavailable")
}
