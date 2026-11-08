package main

import "syscall"

func kernelRelease() (string, error) {
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err != nil {
		return "", err
	}
	release := make([]byte, 0, len(uts.Release))
	for _, c := range uts.Release {
		if c == 0 {
			break
		}
		release = append(release, byte(c))
	}
	return string(release), nil
}

func filesystemUsage(path string) (filesystemCapacity, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return filesystemCapacity{}, err
	}
	blockSize := uint64(s.Frsize)
	if blockSize == 0 {
		blockSize = uint64(s.Bsize)
	}
	result := filesystemCapacity{
		TotalBytes: s.Blocks * blockSize, UsedBytes: (s.Blocks - s.Bfree) * blockSize,
		AvailableBytes: s.Bavail * blockSize, TotalInodes: s.Files, FreeInodes: s.Ffree,
	}
	return result, nil
}
