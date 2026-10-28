package auth

import (
	"cmp"
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	domain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// resetObservationAge accepts any reading taken during the current weekly window: a reset
// time stays correct until it passes, and a passed reset is discarded by the quota view
const resetObservationAge = 8 * 24 * time.Hour

// sessionWindowMax separates short session windows, such as Claude's five-hour limit, from
// weekly ones
const sessionWindowMax = 24 * time.Hour

// OrderedSelector ranks credentials by a configured list of criteria, such as priority and
// quota reset times, and returns the best one; earlier criteria decide and later ones break
// ties, and full ties keep fill-first order. A credential whose fresh quota reading shows an
// exhausted window always ranks last. Unlike the other strategies it ranks every priority
// tier, since priority is one of its criteria. Quota criteria use data from the quota-aware
// chat affinity poller; without it they tie.
type OrderedSelector struct {
	order []string
	quota routingQuotaSource
}

// NewOrderedSelector builds a selector for an order normalized by config.NormalizeRoutingOrder
func NewOrderedSelector(order []string) *OrderedSelector {
	return &OrderedSelector{order: append([]string(nil), order...)}
}

type routingQuotaSource interface {
	routingQuota(auth *Auth, model string) routingQuota
}

// routingQuota is the part of a credential's quota that the quota criteria compare
type routingQuota struct {
	weekly, session windowQuota
	// weight is the credential's quota_weight, which scales remaining fractions across plans
	weight float64
	// exhausted reports a fresh observation of an applicable window with nothing left
	exhausted bool
}

type windowQuota struct {
	// reset is the earliest future reset, or zero when none is known
	reset time.Time
	// remaining is the fraction left, valid when hasRemaining is set
	remaining    float64
	hasRemaining bool
}

// compareResets orders known resets ahead of unknown ones and earlier ahead of later
func compareResets(a, b windowQuota) int {
	switch {
	case a.reset.IsZero() != b.reset.IsZero():
		if b.reset.IsZero() {
			return -1
		}
		return 1
	case a.reset.Before(b.reset):
		return -1
	case b.reset.Before(a.reset):
		return 1
	}
	return 0
}

// compareRemaining orders known remaining quota ahead of unknown and more ahead of less
func compareRemaining(a, b windowQuota, aWeight, bWeight float64) int {
	switch {
	case a.hasRemaining != b.hasRemaining:
		if a.hasRemaining {
			return -1
		}
		return 1
	case a.remaining*aWeight > b.remaining*bWeight:
		return -1
	case a.remaining*aWeight < b.remaining*bWeight:
		return 1
	}
	return 0
}

type orderedCandidate struct {
	auth  *Auth
	quota routingQuota
}

// compare returns a negative number when a should be used ahead of b
func (s *OrderedSelector) compare(a, b orderedCandidate) int {
	if a.quota.exhausted != b.quota.exhausted {
		if b.quota.exhausted {
			return -1
		}
		return 1
	}
	for _, criterion := range s.order {
		var c int
		switch criterion {
		case config.RoutingOrderPriority:
			c = cmp.Compare(authPriority(b.auth), authPriority(a.auth))
		case config.RoutingOrderWeeklyReset:
			c = compareResets(a.quota.weekly, b.quota.weekly)
		case config.RoutingOrderSessionReset:
			c = compareResets(a.quota.session, b.quota.session)
		case config.RoutingOrderWeeklyRemaining:
			c = compareRemaining(a.quota.weekly, b.quota.weekly, a.quota.weight, b.quota.weight)
		case config.RoutingOrderSessionRemaining:
			c = compareRemaining(a.quota.session, b.quota.session, a.quota.weight, b.quota.weight)
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// ranksAllPriorities reports that this selector receives every priority tier
func (s *OrderedSelector) ranksAllPriorities() bool { return true }

// Pick selects the best available credential by the configured order
func (s *OrderedSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	available, err := getSelectorAvailableAuthsAcrossPriorities(ctx, auths, provider, model, time.Now())
	if err != nil {
		return nil, err
	}
	if s == nil {
		return preferCodexWebsocketAuths(ctx, provider, available)[0], nil
	}
	quotas := make(map[*Auth]routingQuota, len(available))
	usable := make([]*Auth, 0, len(available))
	for _, a := range available {
		// without a quota source the quota criteria tie and priority still ranks
		quotas[a] = routingQuota{weight: 1}
		if s.quota != nil {
			quotas[a] = s.quota.routingQuota(a, model)
		}
		if !quotas[a].exhausted {
			usable = append(usable, a)
		}
	}
	// exhaustion outranks the websocket preference: HTTP on a credential with quota beats a
	// websocket on one without
	if len(usable) == 0 {
		usable = available
	}
	usable = preferCodexWebsocketAuths(ctx, provider, usable)
	best := orderedCandidate{auth: usable[0], quota: quotas[usable[0]]}
	for _, a := range usable[1:] {
		if candidate := (orderedCandidate{auth: a, quota: quotas[a]}); s.compare(candidate, best) < 0 {
			best = candidate
		}
	}
	return best.auth, nil
}

// allPrioritySelector is implemented by selectors that rank priority tiers themselves and so
// receive every tier instead of only the highest
type allPrioritySelector interface {
	ranksAllPriorities() bool
}

// selectorCandidates narrows available credentials to what selector should rank
func selectorCandidates(selector Selector, available []*Auth) []*Auth {
	if all, ok := selector.(allPrioritySelector); ok && all.ranksAllPriorities() {
		return available
	}
	return highestPriorityAuths(available)
}

// bindSelectorQuota gives an ordered selector, bare or as a session affinity fallback,
// access to this manager's quota observations
func (m *Manager) bindSelectorQuota(selector Selector) {
	if affinitySelector, ok := selector.(*SessionAffinitySelector); ok && affinitySelector != nil {
		selector = affinitySelector.fallback
	}
	if ordered, ok := selector.(*OrderedSelector); ok && ordered != nil {
		ordered.quota = m
	}
}

func (m *Manager) routingQuota(a *Auth, model string) routingQuota {
	result := routingQuota{weight: quotaWeight(a.Metadata)}
	ref, err := m.credentialRef(a)
	if err != nil {
		return result
	}
	m.affinityMu.Lock()
	r := m.affinityRuntime
	if r == nil {
		m.affinityMu.Unlock()
		return result
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

	// exhaustion and short-window remaining need fresh data, since a stale shortage on a rolling
	// window may have eased; upstream limit errors still put an exhausted credential into cooldown
	fresh := domain.Combine(observations, model, now, maxAge)
	result.exhausted = fresh.Denied()
	for _, w := range fresh.Windows() {
		if isSessionWindow(w) {
			if remaining, ok := w.Remaining().Value(); ok && (!result.session.hasRemaining || remaining < result.session.remaining) {
				result.session.remaining, result.session.hasRemaining = remaining, true
			}
		}
	}
	for _, w := range domain.Combine(observations, model, now, resetObservationAge).Windows() {
		// the tightest weekly window decides remaining quota, with or without a known reset
		if isWeeklyWindow(w) {
			if remaining, ok := w.Remaining().Value(); ok && (!result.weekly.hasRemaining || remaining < result.weekly.remaining) {
				result.weekly.remaining, result.weekly.hasRemaining = remaining, true
			}
		}
		reset, ok := w.ResetAt()
		if !ok || !reset.After(now) {
			continue
		}
		switch {
		case isWeeklyWindow(w):
			if result.weekly.reset.IsZero() || reset.Before(result.weekly.reset) {
				result.weekly.reset = reset
			}
		case isSessionWindow(w):
			if result.session.reset.IsZero() || reset.Before(result.session.reset) {
				result.session.reset = reset
			}
		}
	}
	// live observations start empty after a restart and a rate limited usage read can leave a
	// credential without one for up to an hour; the recorded reset is still correct until it passes
	if result.weekly.reset.IsZero() || !result.weekly.hasRemaining {
		recorded := history.weeklyQuota(a.ID, now, resetObservationAge)
		if result.weekly.reset.IsZero() {
			result.weekly.reset = recorded.reset
		}
		if !result.weekly.hasRemaining {
			result.weekly.remaining, result.weekly.hasRemaining = recorded.remaining, recorded.hasRemaining
		}
	}
	return result
}

func isWeeklyWindow(w domain.Window) bool {
	return w.Period().Duration() == 7*24*time.Hour || w.Period().CalendarKind() == domain.Week
}

func isSessionWindow(w domain.Window) bool {
	d := w.Period().Duration()
	return d > 0 && d < sessionWindowMax
}
