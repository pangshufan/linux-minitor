package configfile

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrecedenceAndStrictConfig(t *testing.T) {
	for _, tt := range []struct {
		name, file string
		args       []string
		env        string
		want       string
		fail       bool
	}{
		{name: "file", file: `{"name":"file","interval":"2s","enabled":false}`, want: "file"},
		{name: "environment", file: `{"name":"file"}`, env: "environment", want: "environment"},
		{name: "cli", file: `{"name":"file"}`, env: "environment", args: []string{"-name=cli"}, want: "cli"},
		{name: "unknown", file: `{"typo":true}`, fail: true},
		{name: "invalid-overridden", file: `{"interval":"oops"}`, args: []string{"-interval=3s"}, fail: true},
		{name: "object", file: `{"name":{}}`, fail: true},
		{name: "trailing", file: `{} {}`, fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.file), 0600); err != nil {
				t.Fatal(err)
			}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			value := fs.String("name", "default", "")
			interval := fs.Duration("interval", time.Second, "")
			enabled := fs.Bool("enabled", true, "")
			if tt.env != "" {
				t.Setenv("TEST_MONITOR_NAME", tt.env)
			}
			err := Parse(fs, append([]string{"-config", path}, tt.args...), map[string]string{"name": "TEST_MONITOR_NAME"})
			if tt.fail {
				if err == nil {
					t.Fatal("expected invalid config")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if *value != tt.want {
				t.Fatalf("name=%q", *value)
			}
			if tt.name == "file" && (*interval != 2*time.Second || *enabled) {
				t.Fatal("typed values not applied")
			}
		})
	}
}
func TestSecretValueNotInConfigError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"secret":"private-value"}`), 0600)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Int("secret", 0, "")
	err := Parse(fs, []string{"-config", path}, nil)
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestStructuredConfigAndCLIOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{"process-services":[{"name":"服务","match":{"executable":"/opt/app"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, override := range []bool{false, true} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		var value JSONValue
		fs.Var(&value, "process-services", "")
		args := []string{"-config", path}
		if override {
			args = append(args, "-process-services=[]")
		}
		if err := Parse(fs, args, nil); err != nil {
			t.Fatal(err)
		}
		if override && value.String() != "[]" {
			t.Fatal("structured CLI override lost")
		}
		if !override && !strings.Contains(value.String(), "/opt/app") {
			t.Fatal("structured file value lost")
		}
	}
}
