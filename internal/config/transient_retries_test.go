package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTransientRetriesConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want int
	}{
		{name: "default", yaml: "routing:\n  strategy: round-robin\n", want: 2},
		{name: "disabled", yaml: "routing:\n  transient-retries: 0\n", want: 0},
		{name: "custom", yaml: "routing:\n  transient-retries: 4\n", want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseConfigBytes([]byte(tc.yaml))
			if err != nil || parsed.Routing.TransientRetries != tc.want {
				t.Fatalf("parse = %v, %v; want %d", parsed, err, tc.want)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil || loaded.Routing.TransientRetries != tc.want {
				t.Fatalf("load = %v, %v; want %d", loaded, err, tc.want)
			}

			// explicit zero must survive save and reload, not turn back into the default
			loaded.Routing.TransientRetries = 0
			if err := SaveConfigPreserveComments(path, loaded); err != nil {
				t.Fatal(err)
			}
			reloaded, err := LoadConfig(path)
			if err != nil || reloaded.Routing.TransientRetries != 0 {
				t.Fatalf("reload = %v, %v; want disabled", reloaded, err)
			}
		})
	}
}
