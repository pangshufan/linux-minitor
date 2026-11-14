package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"monitor/internal/configfile"
	"monitor/internal/transport"
)

type config struct {
	services                             []serviceConfig
	processDiscovery, processPSSInterval time.Duration
	processPSS                           bool
	processMatch                         string
	server, hostID                       string
	tlsEnabled                           bool
	authToken                            string
	processes                            bool
	processNames                         string
	processLimit                         int
	tlsCert, tlsKey, tlsCA               string
	sample, inventory                    time.Duration
	filesystems, push                    time.Duration
	heartbeat                            time.Duration
	windows                              []time.Duration
}

func parseConfig() (config, error) {
	var c config
	var windows string
	var services configfile.JSONValue
	flag.Var(&services, "process-services", "JSON array of named services and selectors")
	flag.DurationVar(&c.processDiscovery, "process-discovery-interval", 5*time.Second, "command selector discovery interval")
	flag.BoolVar(&c.processPSS, "process-pss-enabled", false, "collect proportional memory at a lower frequency")
	flag.DurationVar(&c.processPSSInterval, "process-pss-interval", 30*time.Second, "proportional memory refresh interval")
	flag.BoolVar(&c.tlsEnabled, "tls-enabled", true, "enable gRPC mutual TLS")
	flag.StringVar(&c.authToken, "auth-token", "", "optional shared Agent authentication token")
	flag.BoolVar(&c.processes, "process-enabled", true, "collect only configured services and their processes")
	flag.StringVar(&c.processNames, "process-names", "", "deprecated: explicit comm selectors, converted to named services")
	flag.StringVar(&c.processMatch, "process-match", "", "deprecated: explicit command substrings, converted to named services")
	flag.IntVar(&c.processLimit, "process-limit", 256, "maximum reported processes, ranked by CPU then RSS")
	flag.StringVar(&c.server, "server", "", "central gRPC address")
	flag.StringVar(&c.hostID, "host-id", "", "host ID bound to the client certificate")
	flag.StringVar(&c.tlsCert, "tls-cert", "", "client certificate")
	flag.StringVar(&c.tlsKey, "tls-key", "", "client private key")
	flag.StringVar(&c.tlsCA, "tls-ca", "", "trusted server CA")
	flag.DurationVar(&c.sample, "sample-interval", time.Second, "kernel sampling interval")
	flag.StringVar(&windows, "windows", "", "additional comma-separated calculation windows")
	flag.DurationVar(&c.inventory, "inventory-interval", 5*time.Minute, "hardware and storage discovery interval")
	flag.DurationVar(&c.filesystems, "filesystem-interval", 15*time.Second, "mounted filesystem capacity interval")
	flag.DurationVar(&c.push, "push-interval", 30*time.Second, "snapshot report interval")
	flag.DurationVar(&c.heartbeat, "heartbeat-interval", 5*time.Second, "application heartbeat interval")
	if err := configfile.Parse(flag.CommandLine, os.Args[1:], map[string]string{"auth-token": "MONITOR_AUTH_TOKEN"}); err != nil {
		return c, err
	}

	var err error
	c.services, err = parseServiceConfig(services.String())
	if err != nil {
		return c, err
	}
	if c.processDiscovery <= 0 || c.processPSSInterval <= 0 {
		return c, errors.New("process discovery and PSS intervals must be positive")
	}
	if len(c.services) > 0 && (c.processNames != "" || c.processMatch != "") {
		return c, errors.New("use process-services without legacy process-names/process-match")
	}
	if c.hostID == "" {
		c.hostID = platformHostID()
	}
	if !transport.ValidHostID(c.hostID) {
		return c, errors.New("specify a valid -host-id (letters, digits, dots, underscores or hyphens)")
	}
	if c.processLimit < 1 || c.processLimit > 10000 {
		return c, errors.New("process-limit must be 1–10000")
	}
	if c.server == "" || (c.tlsEnabled && (c.tlsCert == "" || c.tlsKey == "" || c.tlsCA == "")) {
		return c, errors.New("-server, -tls-cert, -tls-key and -tls-ca are required")
	}
	if c.sample <= 0 || c.inventory <= 0 || c.filesystems <= 0 || c.push < time.Second ||
		c.push > 24*time.Hour || c.heartbeat < time.Second || c.heartbeat > time.Hour {
		return c, errors.New("invalid interval (push 1s–24h, heartbeat 1s–1h)")
	}

	c.windows = []time.Duration{c.sample}
	seen := map[time.Duration]bool{c.sample: true}
	if windows != "" {
		for _, value := range strings.Split(windows, ",") {
			d, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil || d < c.sample || d%c.sample != 0 || d/c.sample > 3600 {
				return c, fmt.Errorf("window %q must be a multiple of the sampling interval (1–3600 samples)", value)
			}
			if !seen[d] {
				if len(c.windows) == 8 {
					return c, errors.New("at most eight calculation windows are supported")
				}
				c.windows = append(c.windows, d)
				seen[d] = true
			}
		}
	}
	return c, nil
}

type agent struct {
	hostID      string
	metrics     atomic.Pointer[metricSnapshot]
	inventory   atomic.Pointer[inventorySnapshot]
	filesystems atomic.Pointer[filesystemSnapshot]
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	c, err := parseConfig()
	if err != nil {
		return err
	}
	version, err := checkPlatform(c)
	if err != nil {
		return err
	}
	var tlsConfig *tls.Config
	if c.tlsEnabled {
		tlsConfig, err = transport.TLSConfig(c.tlsCert, c.tlsKey, c.tlsCA)
		if err != nil {
			return err
		}
	}

	return runPlatform(func(ctx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		a := &agent{hostID: c.hostID}
		var collectors sync.WaitGroup
		collectors.Go(func() { a.collectMetrics(ctx, c) })
		collectors.Go(func() { periodically(ctx, c.inventory, true, func() { a.collectInventory(ctx, version) }) })
		collectors.Go(func() { periodically(ctx, c.filesystems, true, a.collectFilesystems) })
		log.Printf("agent host=%s system=%s server=%s", a.hostID, version, c.server)
		err := a.connect(ctx, c, tlsConfig)
		cancel()
		collectors.Wait()
		return err
	})
}

func supportedKernel(release string) bool {
	majorText, rest, ok := strings.Cut(release, ".")
	if !ok {
		return false
	}
	suffix := strings.TrimLeft(rest, "0123456789")
	major, e1 := strconv.Atoi(majorText)
	minor, e2 := strconv.Atoi(strings.TrimSuffix(rest, suffix))
	return e1 == nil && e2 == nil && (major > 4 || major == 4 && minor >= 18)
}

func periodically(ctx context.Context, interval time.Duration, immediate bool, run func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if immediate && ctx.Err() == nil {
		run()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			run()
		}
	}
}
