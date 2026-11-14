//go:build !windows

package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestAgentConfigExamples(t *testing.T) {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine, os.Args = oldFlags, oldArgs }()
	for _, name := range []string{"agent.json", "agent-services.example.json"} {
		flag.CommandLine = flag.NewFlagSet("agent", flag.ContinueOnError)
		os.Args = []string{"agent", "-config", filepath.Join("..", "configs", name), "-tls-enabled=false"}
		cfg, err := parseConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.tlsEnabled || (name == "agent.json" && len(configuredServices(cfg)) != 0) || (name != "agent.json" && len(cfg.services) != 3) {
			t.Fatalf("unexpected config %s: %+v", name, cfg)
		}
	}
}
