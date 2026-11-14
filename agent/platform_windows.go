package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "MonitorAgent"

func platformHostID() string {
	name, _ := os.Hostname()
	return name
}

func checkPlatform(c config) (string, error) {
	if runtime.GOARCH != "amd64" {
		return "", errors.New("Windows Agent currently supports x64")
	}
	v := windows.RtlGetVersion()
	if v.MajorVersion < 10 || v.BuildNumber < 17763 {
		return "", errors.New("Windows Agent requires Windows Server 2019 or Windows 10 version 1809 or later")
	}
	if c.processNames != "" || c.processMatch != "" || c.processPSS {
		return "", errors.New("Windows Agent requires process-services selectors and does not support PSS")
	}
	for _, target := range c.services {
		for _, selector := range append([]serviceMatch{target.Match}, target.MatchAny...) {
			if selector.SystemdUnit != "" || selector.Cgroup != "" || len(selector.ArgsContains) != 0 {
				return "", fmt.Errorf("service %q: Windows supports windows_service and absolute executable selectors", target.Name)
			}
			if selector.Executable != "" && strings.HasPrefix(filepath.Clean(selector.Executable), `\\`) {
				return "", fmt.Errorf("service %q: executable must be on a local drive", target.Name)
			}
		}
	}
	return fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber), nil
}

func runPlatform(run func(context.Context) error) error {
	service, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if service {
		return svc.Run(windowsServiceName, windowsServiceHandler{run: run})
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return run(ctx)
}

type windowsServiceHandler struct {
	run func(context.Context) error
}

func (h windowsServiceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	state := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	statuses <- state
	for {
		select {
		case err := <-done:
			if err != nil {
				log.Printf("Windows Agent: %v", err)
				return false, uint32(windows.ERROR_GEN_FAILURE)
			}
			return false, 0
		case request, ok := <-requests:
			if !ok {
				cancel()
				<-done
				return false, 0
			}
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- state
			case svc.Stop, svc.Shutdown:
				state = svc.Status{State: svc.StopPending, WaitHint: 15000, CheckPoint: 1}
				statuses <- state
				cancel()
			}
		}
	}
}
