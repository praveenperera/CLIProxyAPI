package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	quota "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// orderedFixture returns a manager whose credentials take quota readings at a controllable time
func orderedFixture(t *testing.T) (m *Manager, model string, ids []string, now *time.Time, observe func(id, window string, period time.Duration, reset time.Time, amount float64)) {
	t.Helper()
	m, _, _, model, ids = affinityFixture(t)
	current := time.Now().UTC()
	now = &current
	m.affinityRuntime.now = func() time.Time { return *now }
	return m, model, ids, now, quotaObserver(t, m, now)
}

// quotaObserver records one quota window reading for a credential at *now
func quotaObserver(t *testing.T, m *Manager, now *time.Time) func(id, window string, period time.Duration, reset time.Time, amount float64) {
	return func(id, window string, period time.Duration, reset time.Time, amount float64) {
		t.Helper()
		lease, err := m.QuotaLease(id)
		if err != nil {
			t.Fatal(err)
		}
		scope, _ := quota.SharedCredential("generation")
		rolling, _ := quota.Rolling(period)
		remaining, _ := quota.Fraction(amount, quota.ComparisonKey{})
		// a zero reset records a window whose reset time is unknown
		var resetAt *time.Time
		if !reset.IsZero() {
			resetAt = &reset
		}
		s, err := quota.NewSnapshot(quota.SnapshotInput{Credential: lease.Credential(), Source: quota.ExistingUsageGET, ObservedAt: *now, Windows: []quota.WindowInput{{ID: window, Scope: scope, Period: rolling, ResetAt: resetAt, Remaining: remaining}}})
		if err != nil || !m.ObserveQuota(lease, s) {
			t.Fatalf("observation rejected: %v", err)
		}
	}
}

// useOrder installs the ordered strategy wrapped in session affinity, as the service does
func useOrder(m *Manager, order ...string) {
	m.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: NewOrderedSelector(order)}))
}

func servedBy(t *testing.T, m *Manager, model, step, session string) string {
	t.Helper()
	opts := exec.Options{}
	if session != "" {
		opts.Headers = http.Header{"X-Claude-Code-Session-Id": []string{session}}
	}
	result, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, opts)
	if err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	return string(result.Payload)
}

func TestOrderedWeeklyResetRoutesLegacyRequests(t *testing.T) {
	m, model, ids, nowRef, observe := orderedFixture(t)
	useOrder(m, config.RoutingOrderWeeklyReset)
	now := *nowRef
	servedSession := func(step, session string) string {
		t.Helper()
		*nowRef = now
		return servedBy(t, m, model, step, session)
	}
	served := func(step string) string {
		t.Helper()
		return servedSession(step, "")
	}
	week := 7 * 24 * time.Hour

	if got := served("no quota data"); got != ids[0] {
		t.Fatalf("no quota data served %s, want fill-first %s", got, ids[0])
	}

	observe(ids[0], "seven_day", week, now.Add(5*24*time.Hour), .9)
	observe(ids[1], "seven_day", week, now.Add(2*24*time.Hour), .4)
	if got := served("both observed"); got != ids[1] {
		t.Fatalf("served %s, want earlier weekly reset %s", got, ids[1])
	}
	if got := servedSession("session start", "session-1"); got != ids[1] {
		t.Fatalf("new session served %s, want %s", got, ids[1])
	}

	// a restart empties live observations; the recorded weekly reset still ranks the credential
	m.affinityMu.Lock()
	delete(m.affinityRuntime.observations, ids[1])
	m.affinityMu.Unlock()
	if got := served("restart without live data"); got != ids[1] {
		t.Fatalf("after restart served %s, want recorded earlier reset %s", got, ids[1])
	}

	// usage polls can pause for an hour under rate limiting; the reset time is still right
	now = now.Add(time.Hour)
	if got := served("stale observations"); got != ids[1] {
		t.Fatalf("stale observations served %s, want %s", got, ids[1])
	}

	observe(ids[1], "five_hour", 5*time.Hour, now.Add(time.Hour), 0)
	if got := served("session window exhausted"); got != ids[0] {
		t.Fatalf("exhausted session window served %s, want %s", got, ids[0])
	}
	// a bound session keeps its warm cache until its credential actually fails or cools down
	if got := servedSession("bound session", "session-1"); got != ids[1] {
		t.Fatalf("bound session moved to %s, want %s", got, ids[1])
	}

	now = now.Add(2 * time.Hour)
	if got := served("session window reset"); got != ids[1] {
		t.Fatalf("after session reset served %s, want %s", got, ids[1])
	}

	now = now.Add(2 * 24 * time.Hour)
	if got := served("weekly reset passed"); got != ids[0] {
		t.Fatalf("after weekly reset served %s, want %s", got, ids[0])
	}

	// strict chats require a built-in selector for their fallback
	r := affinityRequest(t, m, "caller", "weekly-chat", affinity.StartIntent, affinity.Handle{})
	if _, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: r}); err != nil {
		t.Fatalf("strict chat start: %v", err)
	}
}

func TestOrderedCombinesCriteriaAcrossPriorityTiers(t *testing.T) {
	m, model, ids, nowRef, observe := orderedFixture(t)
	now := *nowRef
	week := 7 * 24 * time.Hour
	// ids[0] is the higher priority tier on a smaller plan; ids[1] resets its week first
	for i, settings := range []struct {
		priority string
		weight   float64
	}{{"1", 5}, {"0", 10}} {
		a, _ := m.GetByID(ids[i])
		a.Attributes["priority"] = settings.priority
		if a.Metadata == nil {
			a.Metadata = map[string]any{}
		}
		a.Metadata["quota_weight"] = settings.weight
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	observe(ids[0], "seven_day", week, now.Add(5*24*time.Hour), .5)
	observe(ids[1], "seven_day", week, now.Add(2*24*time.Hour), .3)
	observe(ids[0], "five_hour", 5*time.Hour, now.Add(time.Hour), .2)
	observe(ids[1], "five_hour", 5*time.Hour, now.Add(3*time.Hour), .5)

	for _, tc := range []struct {
		name  string
		order []string
		want  string
	}{
		{"priority decides first", []string{config.RoutingOrderPriority, config.RoutingOrderWeeklyReset}, ids[0]},
		{"weekly reset outranks priority tier", []string{config.RoutingOrderWeeklyReset, config.RoutingOrderPriority}, ids[1]},
		// .3 of a weight-10 plan is more quota than .5 of a weight-5 plan
		{"weekly remaining scales by plan", []string{config.RoutingOrderWeeklyRemaining}, ids[1]},
		{"session reset", []string{config.RoutingOrderSessionReset, config.RoutingOrderWeeklyReset}, ids[0]},
		{"session remaining", []string{config.RoutingOrderSessionRemaining, config.RoutingOrderPriority}, ids[1]},
	} {
		useOrder(m, tc.order...)
		if got := servedBy(t, m, model, tc.name, ""); got != tc.want {
			t.Errorf("%s: served %s, want %s", tc.name, got, tc.want)
		}
	}

	// a bound session never follows a change in ranking, so its prompt cache stays warm
	useOrder(m, config.RoutingOrderPriority)
	if got := servedBy(t, m, model, "bind", "session-1"); got != ids[0] {
		t.Fatalf("session bound to %s, want %s", got, ids[0])
	}
	// saving a new order rebuilds the selector; the binding must survive it
	useOrder(m, config.RoutingOrderWeeklyReset)
	if got := servedBy(t, m, model, "after order change", "session-1"); got != ids[0] {
		t.Fatalf("order change moved bound session to %s", got)
	}
	if got := servedBy(t, m, model, "new session after order change", ""); got != ids[1] {
		t.Fatalf("new order not applied: served %s", got)
	}
	// readings must be newer than the last one to replace it
	*nowRef = now.Add(time.Minute)
	observe(ids[0], "five_hour", 5*time.Hour, now.Add(time.Hour), 0)
	if got := servedBy(t, m, model, "bound session after exhaustion", "session-1"); got != ids[0] {
		t.Fatalf("bound session moved to %s before its credential failed", got)
	}
	if got := servedBy(t, m, model, "new session after exhaustion", ""); got != ids[1] {
		t.Fatalf("exhausted credential served new request: %s", got)
	}
}

func TestOrderedQuotaEdgeCases(t *testing.T) {
	m, model, ids, nowRef, observe := orderedFixture(t)
	now := *nowRef
	week := 7 * 24 * time.Hour

	// a measured weekly remaining counts even when the reading has no reset time
	observe(ids[0], "seven_day", week, now.Add(2*24*time.Hour), .1)
	observe(ids[1], "seven_day", week, time.Time{}, .9)
	useOrder(m, config.RoutingOrderWeeklyRemaining)
	if got := servedBy(t, m, model, "remaining without reset", ""); got != ids[1] {
		t.Fatalf("served %s, want the credential with more weekly quota %s", got, ids[1])
	}

	// exhaustion outranks the websocket preference, since the other credential can serve over HTTP
	a, _ := m.GetByID(ids[1])
	a.Attributes["websockets"] = "true"
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	*nowRef = now.Add(time.Minute)
	observe(ids[1], "five_hour", 5*time.Hour, now.Add(time.Hour), 0)
	ctx := exec.WithDownstreamWebsocket(context.Background())
	result, err := m.Execute(ctx, []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Payload); got != ids[0] {
		t.Fatalf("websocket request served by exhausted %s, want %s", got, ids[0])
	}
}

func TestOrderedStrictChatFallbackRanksAllTiers(t *testing.T) {
	m, model, ids, nowRef, observe := orderedFixture(t)
	now := *nowRef
	a, _ := m.GetByID(ids[0])
	a.Attributes["priority"] = "10"
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	observe(ids[0], "seven_day", 7*24*time.Hour, now.Add(5*24*time.Hour), .5)
	observe(ids[1], "seven_day", 7*24*time.Hour, now.Add(24*time.Hour), .5)
	useOrder(m, config.RoutingOrderWeeklyReset)
	// stale readings leave strict ranking without data, while the weekly resets stay valid
	*nowRef = now.Add(time.Hour)

	r := affinityRequest(t, m, "caller", "ordered-chat", affinity.StartIntent, affinity.Handle{})
	result, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: r})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Payload); got != ids[1] {
		t.Fatalf("strict chat fallback chose %s, want lower-tier earlier reset %s", got, ids[1])
	}
}

func TestOrderedRanksPriorityWithoutQuotaSource(t *testing.T) {
	auths := []*Auth{{ID: "a", Attributes: map[string]string{"priority": "0"}}, {ID: "z", Attributes: map[string]string{"priority": "10"}}}
	got, err := NewOrderedSelector([]string{config.RoutingOrderPriority}).Pick(context.Background(), "gemini", "", exec.Options{}, auths)
	if err != nil || got == nil || got.ID != "z" {
		t.Fatalf("Pick() = %v, %v; want the higher-priority z", got, err)
	}
}

func TestOrderedRecordedWeeklyRemainingMatchesLiveRules(t *testing.T) {
	m, model, ids, nowRef, observe := orderedFixture(t)
	now := *nowRef
	week := 7 * 24 * time.Hour
	// ids[0]'s soonest-resetting weekly window has plenty left, but another weekly window is nearly out
	observe(ids[0], "seven_day", week, now.Add(24*time.Hour), .9)
	observe(ids[0], "seven_day_opus", week, now.Add(3*24*time.Hour), .1)
	observe(ids[1], "seven_day", week, now.Add(2*24*time.Hour), .5)
	useOrder(m, config.RoutingOrderWeeklyRemaining)
	if got := servedBy(t, m, model, "live", ""); got != ids[1] {
		t.Fatalf("live readings served %s, want %s", got, ids[1])
	}

	// a restart empties live observations; the recorded points must rank the same way
	m.affinityMu.Lock()
	m.affinityRuntime.observations = map[string]quotaRecord{}
	m.affinityMu.Unlock()
	if got := servedBy(t, m, model, "after restart", ""); got != ids[1] {
		t.Fatalf("recorded readings served %s, want %s", got, ids[1])
	}
}

func TestSessionAffinityKeepsCodexWebsocketPreference(t *testing.T) {
	m, _, _, model, ids := affinityFixture(t)
	m.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}}))
	a, _ := m.GetByID(ids[1])
	a.Attributes["websockets"] = "true"
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	ctx := exec.WithDownstreamWebsocket(context.Background())
	result, err := m.Execute(ctx, []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Payload); got != ids[1] {
		t.Fatalf("websocket request served by %s, want websocket credential %s", got, ids[1])
	}
}
