package auth

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	domain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// weeklyObservationAge accepts any reading taken during the current weekly window: a reset
// time stays correct until it passes, and a passed reset is discarded by the quota view
const weeklyObservationAge = 8 * 24 * time.Hour

// WeeklyResetFirstSelector sends traffic to the credential whose weekly quota resets soonest,
// so allowance that would expire unused is spent first, and keeps it there until that
// credential is exhausted or cooling down. Credentials without weekly quota data come after
// those with it, in fill-first order. Quota data comes from the quota-aware chat affinity
// poller; without it the selector behaves like fill-first.
type WeeklyResetFirstSelector struct {
	quota weeklyQuotaSource
}

type weeklyQuotaSource interface {
	weeklyQuota(auth *Auth, model string) weeklyQuota
}

// weeklyQuota is the part of a credential's quota that decides weekly-reset-first order
type weeklyQuota struct {
	// reset is the earliest future weekly reset, or zero when none is known
	reset time.Time
	// remaining is the weekly fraction left at reset's window, valid when hasRemaining is set
	remaining    float64
	hasRemaining bool
	// exhausted reports a fresh observation of an applicable window with nothing left
	exhausted bool
}

// before reports whether q should be used ahead of other
func (q weeklyQuota) before(other weeklyQuota) bool {
	if q.exhausted != other.exhausted {
		return other.exhausted
	}
	if q.reset.IsZero() != other.reset.IsZero() {
		return other.reset.IsZero()
	}
	if !q.reset.Equal(other.reset) {
		return q.reset.Before(other.reset)
	}
	return q.hasRemaining && (!other.hasRemaining || q.remaining > other.remaining)
}

// Pick selects the available credential with the earliest weekly reset
func (s *WeeklyResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, time.Now())
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if s == nil || s.quota == nil {
		return available[0], nil
	}
	best, bestQuota := available[0], s.quota.weeklyQuota(available[0], model)
	for _, candidate := range available[1:] {
		if quota := s.quota.weeklyQuota(candidate, model); quota.before(bestQuota) {
			best, bestQuota = candidate, quota
		}
	}
	return best, nil
}

// bindSelectorQuota gives a weekly-reset-first selector, bare or as a session affinity
// fallback, access to this manager's quota observations
func (m *Manager) bindSelectorQuota(selector Selector) {
	if affinitySelector, ok := selector.(*SessionAffinitySelector); ok && affinitySelector != nil {
		selector = affinitySelector.fallback
	}
	if weekly, ok := selector.(*WeeklyResetFirstSelector); ok && weekly != nil {
		weekly.quota = m
	}
}

func (m *Manager) weeklyQuota(a *Auth, model string) weeklyQuota {
	ref, err := m.credentialRef(a)
	if err != nil {
		return weeklyQuota{}
	}
	m.affinityMu.Lock()
	r := m.affinityRuntime
	if r == nil {
		m.affinityMu.Unlock()
		return weeklyQuota{}
	}
	now := r.now()
	history := r.history
	var observations []domain.Snapshot
	if record := r.observations[a.ID]; record.ref == ref {
		for _, s := range record.windows {
			observations = append(observations, s)
		}
	}
	maxAge := r.settings.MaxQuotaAge
	m.affinityMu.Unlock()

	// exhaustion needs fresh data, since a stale shortage on a rolling window may have eased;
	// upstream limit errors still put an exhausted credential into cooldown either way
	result := weeklyQuota{exhausted: domain.Combine(observations, model, now, maxAge).Denied()}
	for _, w := range domain.Combine(observations, model, now, weeklyObservationAge).Windows() {
		if w.Period().Duration() != 7*24*time.Hour && w.Period().CalendarKind() != domain.Week {
			continue
		}
		reset, ok := w.ResetAt()
		if !ok || !reset.After(now) {
			continue
		}
		if result.reset.IsZero() || reset.Before(result.reset) {
			result.reset = reset
			result.remaining, result.hasRemaining = w.Remaining().Value()
		}
	}
	// live observations start empty after a restart and a rate limited usage read can leave a
	// credential without one for up to an hour; the recorded reset is still correct until it passes
	if result.reset.IsZero() {
		result.reset, result.remaining, result.hasRemaining = history.weeklyReset(a.ID, now)
	}
	return result
}
