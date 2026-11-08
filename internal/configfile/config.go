// Package configfile loads JSON configuration using the same names and values as CLI flags.
package configfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// Parse applies defaults < JSON file < environment overrides < CLI arguments.
// Paths in the file are relative to the process working directory, like CLI paths.
func Parse(fs *flag.FlagSet, args []string, env map[string]string) error {
	var path string
	fs.StringVar(&path, "config", "", "JSON configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var values map[string]any
		if err := dec.Decode(&values); err != nil || values == nil {
			return errors.New("config must be a JSON object")
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return errors.New("unexpected content after config object")
		}
		for name, value := range values {
			f := fs.Lookup(name)
			if f == nil || name == "config" {
				return fmt.Errorf("unknown config key %q", name)
			}
			var text string
			switch v := value.(type) {
			case string:
				text = v
			case []any, map[string]any:
				if _, ok := f.Value.(*JSONValue); !ok {
					return fmt.Errorf("config key %q does not accept structured values", name)
				}
				data, err := json.Marshal(v)
				if err != nil {
					return err
				}
				text = string(data)
			case bool, json.Number:
				text = fmt.Sprint(v)
			default:
				return fmt.Errorf("config key %q requires a string, boolean or number", name)
			}
			// Validate even overridden values, without changing the effective CLI value.
			old := f.Value.String()
			if err := fs.Set(name, text); err != nil {
				return fmt.Errorf("invalid config value for %q", name)
			}
			if explicit[name] {
				if err := fs.Set(name, old); err != nil {
					return err
				}
			}
		}
	}
	for name, variable := range env {
		if value, ok := os.LookupEnv(variable); ok && !explicit[name] {
			if err := fs.Set(name, value); err != nil {
				return fmt.Errorf("invalid environment value for %q", name)
			}
		}
	}
	return nil
}

// JSONValue allows a flag to receive a nested JSON value from files or the CLI.
type JSONValue string

func (v *JSONValue) String() string {
	if v == nil || *v == "" {
		return "[]"
	}
	return string(*v)
}
func (v *JSONValue) Set(text string) error {
	if !json.Valid([]byte(text)) {
		return errors.New("invalid JSON")
	}
	*v = JSONValue(text)
	return nil
}
