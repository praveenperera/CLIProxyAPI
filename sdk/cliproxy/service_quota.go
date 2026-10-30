package cliproxy

import (
	"context"
	"strings"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// validateForkConfig rejects a config update whose fork settings are invalid or would change
// the quota-aware chat affinity profile, which needs a restart
func (s *Service) validateForkConfig(newCfg *config.Config) bool {
	if errValidate := newCfg.ValidateRoutingOrder(); errValidate != nil {
		log.WithError(errValidate).Warn("rejected config update with invalid routing order")
		return false
	}
	if _, errValidate := newCfg.QuotaAffinitySettings(); errValidate != nil {
		log.WithError(errValidate).Warn("rejected quota affinity config update")
		return false
	}
	s.cfgMu.RLock()
	previous := s.cfg
	s.cfgMu.RUnlock()
	oldProfile, errOld := previous.QuotaAffinitySettings()
	newProfile, _ := newCfg.QuotaAffinitySettings()
	if errOld != nil || oldProfile != newProfile {
		log.Warn("rejected quota affinity profile change; restart required")
		return false
	}
	if s.coreManager != nil && s.coreManager.QuotaAffinityNamespace() != "" {
		if errValidate := s.coreManager.ValidateQuotaAffinityReload(newCfg); errValidate != nil {
			log.WithError(errValidate).Warn("rejected quota affinity reload")
			return false
		}
	}
	return true
}

// normalizedRoutingOrder joins the ordered strategy's criteria with commas, keeping the routing
// state comparable
func normalizedRoutingOrder(cfg *config.Config) string {
	// invalid orders are rejected before a config is applied
	order, _ := config.NormalizeRoutingOrder(cfg.Routing.Order)
	return strings.Join(order, ",")
}

// newOrderedSelector builds the ordered strategy, warning when no quota data feeds its criteria
func newOrderedSelector(state routingRuntimeState) coreauth.Selector {
	selector := coreauth.NewOrderedSelector(strings.Split(state.order, ","))
	if !state.quotaPolling {
		log.Warn("routing strategy ordered has no quota data without routing.quota-aware-chat-affinity; quota criteria tie")
	}
	return selector
}

// quotaPoller tracks the quota poller goroutine so shutdown can stop it and wait for it
type quotaPoller struct {
	quotaMu         sync.Mutex
	quotaStopped    bool
	quotaPollCancel context.CancelFunc
	quotaPollWG     sync.WaitGroup
}

// initQuotaAffinity validates the quota-aware chat affinity profile, configures it on the core
// manager, and registers the cache usage plugin before the usage dispatcher starts
func (s *Service) initQuotaAffinity() error {
	if _, errProfile := s.cfg.QuotaAffinitySettings(); errProfile != nil {
		return errProfile
	}
	if s.coreManager != nil {
		if errProfile := s.coreManager.ConfigureQuotaAffinity(s.cfg); errProfile != nil {
			return errProfile
		}
		usage.RegisterNamedPlugin("cache-usage", s.coreManager.CacheUsagePlugin())
	}
	return nil
}

// finishQuotaAffinity runs after the usage dispatcher stops; a close error becomes the
// shutdown error unless one is already set
func (s *Service) finishQuotaAffinity(ctx context.Context, shutdownErr *error) {
	// queued records reach the cache usage plugin before its final snapshot
	if errWait := usage.WaitDefault(ctx); errWait != nil {
		log.WithError(errWait).Warn("usage records still queued at shutdown were not delivered")
	}
	if s.coreManager != nil {
		if errClose := s.coreManager.CloseQuotaAffinity(); errClose != nil && *shutdownErr == nil {
			*shutdownErr = errClose
		}
	}
}

func (s *Service) startQuotaPoller(ctx context.Context) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.quotaStopped || s.quotaPollCancel != nil || s.coreManager == nil || s.coreManager.QuotaAffinityNamespace() == "" {
		return
	}
	pollCtx, cancel := context.WithCancel(ctx)
	s.quotaPollCancel = cancel
	s.quotaPollWG.Add(1)
	go func() { defer s.quotaPollWG.Done(); s.coreManager.RunQuotaPoller(pollCtx) }()
}

func (s *Service) stopQuotaPoller() {
	s.quotaMu.Lock()
	s.quotaStopped = true
	cancel := s.quotaPollCancel
	s.quotaMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.quotaPollWG.Wait()
}
