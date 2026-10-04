package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuotaAffinitySharedStartupValidation(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	raw := "routing:\n  quota-aware-chat-affinity:\n    namespace: example\n    state-dir: " + state + "\n"
	for _, prefix := range []string{"", "config-version: 8\n"} {
		cfg, err := ParseConfigBytes([]byte(prefix + raw))
		if err != nil {
			t.Fatal(err)
		}
		settings, err := cfg.QuotaAffinitySettings()
		if err != nil {
			t.Fatal(err)
		}
		if settings.IdleRetention != 7*24*time.Hour || settings.HardRetention != 30*24*time.Hour || settings.MaxBindings != 65536 || settings.PollInterval != time.Minute || settings.MaxQuotaAge != 5*time.Minute || settings.PollConcurrency != 4 {
			t.Fatalf("wrong defaults %+v", settings)
		}
		file := filepath.Join(t.TempDir(), "example.yaml")
		if err = os.WriteFile(file, []byte(prefix+raw), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		loadedSettings, err := loaded.QuotaAffinitySettings()
		if err != nil || loadedSettings != settings {
			t.Fatal("startup/load validation differs")
		}
	}
	for _, extra := range []string{"    namespace: ''\n", "    state-dir: relative\n", "    idle-retention: -1h\n", "    hard-retention: 1h\n", "    max-bindings: 65537\n", "    poll-concurrency: 33\n", "    poll-interval: 0s\n", "    poll-timeout: 30s\n", "    max-quota-age: 25h\n", "    invented-remaining: 1\n"} {
		// replace the target field to avoid duplicate YAML keys obscuring semantic validation
		candidate := raw
		if strings.HasPrefix(extra, "    namespace:") {
			candidate = strings.Replace(candidate, "    namespace: example\n", "", 1)
		}
		if strings.HasPrefix(extra, "    state-dir:") {
			candidate = strings.Replace(candidate, "    state-dir: "+state+"\n", "", 1)
		}
		candidate += extra
		if _, err := ParseConfigBytes([]byte(candidate)); err == nil {
			t.Fatalf("accepted invalid config %s", extra)
		}
	}
	cfg := &Config{Routing: RoutingConfig{QuotaAwareChatAffinity: &QuotaAffinityConfig{Namespace: "example", StateDir: state}}}
	for _, mutate := range []func(*Config){func(c *Config) { c.Home.Enabled = true }, func(c *Config) { c.Plugins.Enabled = true }, func(c *Config) { c.Routing.SessionAffinity = true }, func(c *Config) { c.Routing.Strategy = "invalid" }, func(c *Config) { c.AuthDir = filepath.Dir(state) }} {
		copy := *cfg
		mutate(&copy)
		if _, err := copy.QuotaAffinitySettings(); err == nil {
			t.Fatal("unsupported ownership accepted")
		}
	}
	if _, err := (&Config{}).QuotaAffinitySettings(); err != nil {
		t.Fatal("legacy defaults changed")
	}
}

func TestQuotaAffinityRejectsAuthWatcherAliases(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(authDir, alias); err != nil {
		t.Fatal(err)
	}
	for _, dirs := range [][2]string{{authDir, filepath.Join(alias, "new-state")}, {alias, filepath.Join(authDir, "new-state")}} {
		cfg := &Config{AuthDir: dirs[0], Routing: RoutingConfig{QuotaAwareChatAffinity: &QuotaAffinityConfig{Namespace: "test", StateDir: dirs[1]}}}
		if _, err := cfg.QuotaAffinitySettings(); err == nil {
			t.Fatal("state inside aliased auth watcher accepted")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{AuthDir: "~/.cli-proxy-api", Routing: RoutingConfig{QuotaAwareChatAffinity: &QuotaAffinityConfig{Namespace: "test", StateDir: filepath.Join(home, ".cli-proxy-api", "state")}}}
	if _, err := cfg.QuotaAffinitySettings(); err == nil {
		t.Fatal("home-relative auth watcher containment accepted")
	}
}
