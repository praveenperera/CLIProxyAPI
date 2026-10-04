package cliproxy

import (
	"context"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestQuotaAffinityServiceReloadKeepsValidRuntime(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.QuotaAwareChatAffinity = &config.QuotaAffinityConfig{Namespace: "test", StateDir: filepath.Join(t.TempDir(), "state")}
	manager := coreauth.NewManager(nil, nil, nil)
	if err := manager.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.CloseQuotaAffinity() }()
	s := &Service{cfg: cfg, coreManager: manager}
	unchanged := *cfg
	unchanged.Host = "0.0.0.0"
	if commit := s.commitConfigUpdate(&unchanged); commit.cfg == nil {
		t.Fatal("unchanged profile rejected")
	}
	for _, next := range []*config.Config{{}, func() *config.Config {
		copy := unchanged
		profile := *copy.Routing.QuotaAwareChatAffinity
		profile.StateDir = filepath.Join(t.TempDir(), "other")
		copy.Routing.QuotaAwareChatAffinity = &profile
		return &copy
	}(), func() *config.Config { copy := unchanged; copy.Home.Enabled = true; return &copy }()} {
		if commit := s.commitConfigUpdate(next); commit.cfg != nil {
			t.Fatal("unsafe reload committed")
		}
		if s.cfg != &unchanged || manager.QuotaAffinityNamespace() != "test" {
			t.Fatal("invalid reload changed runtime")
		}
	}
	s.startQuotaPoller(context.Background())
	s.stopQuotaPoller()
	s.startQuotaPoller(context.Background())
	if !s.quotaStopped {
		t.Fatal("poller restarted after stop")
	}
}
