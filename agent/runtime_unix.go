//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

func platformHostID() string {
	if id := firstText("/etc/machine-id", "/var/lib/dbus/machine-id"); id != "" {
		return id
	}
	name, _ := os.Hostname()
	return name
}

func checkPlatform(c config) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("monitor agent requires Linux or Windows")
	}
	for _, target := range c.services {
		if target.Match.WindowsService != "" {
			return "", fmt.Errorf("service %q uses a Windows selector", target.Name)
		}
	}
	version, err := kernelRelease()
	if err != nil {
		return "", err
	}
	if !supportedKernel(version) {
		return "", fmt.Errorf("Linux kernel >= 4.18 is required, found %q", version)
	}
	return version, nil
}

func runPlatform(run func(context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}
