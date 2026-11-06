package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatHistoryLoadsFromV8Layout(t *testing.T) {
	load := func(block string) (*Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		body := "config-version: 8\nobservability:\n  chat-history:\n" + block
		if errWrite := os.WriteFile(path, []byte(body), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}

		return LoadConfig(path)
	}

	cfg, errLoad := load(`    enabled: true
    endpoint: "http://code.local:8123"
    user: "chat"
    password-file: "~/.config/chat-history/password"
    spool-dir: "~/.local/share/cliproxyapi/chat-history"
    flush-seconds: 10
`)
	if errLoad != nil {
		t.Fatalf("load: %v", errLoad)
	}

	got := cfg.ChatHistory
	if got == nil || !got.Enabled || got.Endpoint != "http://code.local:8123" || got.User != "chat" || got.FlushSeconds != 10 {
		t.Fatalf("chat-history = %+v", got)
	}

	if _, errLoad = load("    enabled: true\n    endpoint: \"code.local\"\n"); errLoad == nil ||
		!strings.Contains(errLoad.Error(), "endpoint") || !strings.Contains(errLoad.Error(), "spool-dir") {
		t.Fatalf("incomplete block error = %v, want endpoint and spool-dir problems", errLoad)
	}

	if _, errLoad = load("    enabled: false\n"); errLoad != nil {
		t.Fatalf("a disabled block needs no other fields: %v", errLoad)
	}
}
