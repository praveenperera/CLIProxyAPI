package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ChatHistoryConfig sends every proxied request and response to ClickHouse; see
// docs/chat-history.md
type ChatHistoryConfig struct {
	// Enabled turns capture on; the other fields are ignored while it is false
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Endpoint is the ClickHouse HTTP URL, such as http://code.local:8123
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// Database defaults to "chat"
	Database string `yaml:"database,omitempty" json:"database,omitempty"`
	// User is the ClickHouse user
	User string `yaml:"user" json:"user"`
	// PasswordFile holds the ClickHouse password, so the password stays out of this
	// file and out of the management API's config view
	PasswordFile string `yaml:"password-file" json:"password-file"`
	// SpoolDir holds rows waiting for upload; it must not be shared by two servers
	SpoolDir string `yaml:"spool-dir" json:"spool-dir"`
	// MaxSpoolMB stops capture while this much is waiting for upload, so a long
	// ClickHouse outage cannot fill the disk. Default: 5120; zero means the default
	MaxSpoolMB int64 `yaml:"max-spool-mb,omitempty" json:"max-spool-mb,omitempty"`
	// FlushSeconds bounds how long a row waits before upload. Default: 30
	FlushSeconds int `yaml:"flush-seconds,omitempty" json:"flush-seconds,omitempty"`
}

// ValidateChatHistory checks the chat-history block when it is enabled
func (cfg *Config) ValidateChatHistory() error {
	if cfg == nil || cfg.ChatHistory == nil || !cfg.ChatHistory.Enabled {
		return nil
	}

	settings := cfg.ChatHistory
	var problems []string
	if parsed, errParse := url.Parse(strings.TrimSpace(settings.Endpoint)); errParse != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		problems = append(problems, "endpoint must be an http or https URL")
	}

	if strings.TrimSpace(settings.User) == "" {
		problems = append(problems, "user is required")
	}

	if strings.TrimSpace(settings.PasswordFile) == "" {
		problems = append(problems, "password-file is required")
	}

	if strings.TrimSpace(settings.SpoolDir) == "" {
		problems = append(problems, "spool-dir is required")
	}

	if settings.MaxSpoolMB < 0 || settings.FlushSeconds < 0 {
		problems = append(problems, "max-spool-mb and flush-seconds must not be negative")
	}

	if database := settings.Database; database != "" && !isIdentifier(database) {
		problems = append(problems, "database must be letters, digits and underscores")
	}

	if len(problems) > 0 {
		return fmt.Errorf("chat-history: %w", errors.New(strings.Join(problems, "; ")))
	}

	return nil
}

func isIdentifier(name string) bool {
	for _, r := range name {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}

	return name != ""
}
