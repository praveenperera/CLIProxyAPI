package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// QuotaAffinityConfig enables local durable chat routing without changing legacy requests
// Empty fields use bounded defaults except namespace and state directory
type QuotaAffinityConfig struct {
	Namespace       string `yaml:"namespace" json:"namespace"`
	StateDir        string `yaml:"state-dir" json:"state-dir"`
	IdleRetention   string `yaml:"idle-retention,omitempty" json:"idle-retention,omitempty"`
	HardRetention   string `yaml:"hard-retention,omitempty" json:"hard-retention,omitempty"`
	MaxBindings     int    `yaml:"max-bindings,omitempty" json:"max-bindings,omitempty"`
	MaxQuotaAge     string `yaml:"max-quota-age,omitempty" json:"max-quota-age,omitempty"`
	PollInterval    string `yaml:"poll-interval,omitempty" json:"poll-interval,omitempty"`
	PollConcurrency int    `yaml:"poll-concurrency,omitempty" json:"poll-concurrency,omitempty"`
}

// QuotaAffinitySettings is the validated runtime profile
type QuotaAffinitySettings struct {
	Namespace, StateDir                                     string
	IdleRetention, HardRetention, MaxQuotaAge, PollInterval time.Duration
	MaxBindings, PollConcurrency                            int
}

func affinityDuration(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid quota affinity duration")
	}
	return d, nil
}

// QuotaAffinitySettings validates the same profile for startup and reload
func (cfg *Config) QuotaAffinitySettings() (QuotaAffinitySettings, error) {
	var s QuotaAffinitySettings
	if cfg == nil || cfg.Routing.QuotaAwareChatAffinity == nil {
		return s, nil
	}
	p := cfg.Routing.QuotaAwareChatAffinity
	switch strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy)) {
	case "", "round-robin", "weighted-round-robin", "weightedroundrobin", "wrr", "fill-first", "fillfirst", "ff":
	default:
		return s, fmt.Errorf("quota affinity requires a supported fallback strategy")
	}
	if cfg.Home.Enabled || cfg.Plugins.Enabled || cfg.Routing.SessionAffinity {
		return s, fmt.Errorf("quota affinity requires local built-in routing without Home, plugins, or soft affinity")
	}
	if !utf8.ValidString(p.Namespace) || p.Namespace == "" || len(p.Namespace) > 256 || strings.TrimSpace(p.Namespace) != p.Namespace || strings.ContainsAny(p.Namespace, "\r\n\x00") {
		return s, fmt.Errorf("quota affinity requires a valid namespace")
	}
	if !filepath.IsAbs(p.StateDir) {
		return s, fmt.Errorf("quota affinity requires an absolute state directory")
	}
	for _, r := range p.Namespace {
		if unicode.IsControl(r) {
			return s, fmt.Errorf("quota affinity requires a valid namespace")
		}
	}
	s.Namespace = p.Namespace
	s.StateDir = filepath.Clean(p.StateDir)
	authPath := cfg.AuthDir
	if authPath == "" {
		authPath = DefaultAuthDir
	}
	if strings.HasPrefix(authPath, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return s, fmt.Errorf("quota affinity cannot resolve auth directory")
		}
		remainder := strings.TrimLeft(strings.TrimPrefix(authPath, "~"), "/\\")
		authPath = filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(remainder, "\\", "/")))
	}
	authDir, errPath := resolvedAffinityPath(authPath)
	stateDir, errState := resolvedAffinityPath(s.StateDir)
	if errPath != nil || errState != nil {
		return s, fmt.Errorf("quota affinity cannot resolve state and auth directories")
	}
	rel, errRel := filepath.Rel(authDir, stateDir)
	if errRel != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return s, fmt.Errorf("quota affinity state directory must be outside auth directory")
	}
	var err error
	if s.IdleRetention, err = affinityDuration(p.IdleRetention, 7*24*time.Hour); err != nil {
		return s, err
	}
	if s.HardRetention, err = affinityDuration(p.HardRetention, 30*24*time.Hour); err != nil {
		return s, err
	}
	if s.MaxQuotaAge, err = affinityDuration(p.MaxQuotaAge, 5*time.Minute); err != nil {
		return s, err
	}
	if s.PollInterval, err = affinityDuration(p.PollInterval, time.Minute); err != nil {
		return s, err
	}
	s.MaxBindings = p.MaxBindings
	if s.MaxBindings == 0 {
		s.MaxBindings = 65536
	}
	s.PollConcurrency = p.PollConcurrency
	if s.PollConcurrency == 0 {
		s.PollConcurrency = 4
	}
	if s.HardRetention < s.IdleRetention || s.HardRetention > 365*24*time.Hour || s.MaxBindings < 1 || s.MaxBindings > 65536 || s.PollConcurrency < 1 || s.PollConcurrency > 32 || s.PollInterval < time.Second || s.PollInterval > 24*time.Hour || s.MaxQuotaAge > 24*time.Hour {
		return s, fmt.Errorf("quota affinity settings exceed supported bounds")
	}
	return s, nil
}

// UnmarshalYAML rejects unknown strict profile fields instead of silently using defaults
func (p *QuotaAffinityConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("quota affinity profile must be a mapping")
	}
	known := map[string]bool{"namespace": true, "state-dir": true, "idle-retention": true, "hard-retention": true, "max-bindings": true, "max-quota-age": true, "poll-interval": true, "poll-concurrency": true}
	for i := 0; i < len(node.Content); i += 2 {
		if !known[node.Content[i].Value] {
			return fmt.Errorf("unknown quota affinity field")
		}
	}
	type plain QuotaAffinityConfig
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*p = QuotaAffinityConfig(value)
	return nil
}

// resolve existing ancestors too, because a new state directory can have a symlink parent
func resolvedAffinityPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			suffix, err := filepath.Rel(current, absolute)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		current = parent
	}
}
