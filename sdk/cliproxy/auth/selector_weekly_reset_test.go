package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	quota "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

func TestWeeklyResetFirstRoutesLegacyRequestsByWeeklyReset(t *testing.T) {
	m, _, _, model, ids := affinityFixture(t)
	// the service always wraps the strategy in session affinity
	m.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &WeeklyResetFirstSelector{}}))
	now := time.Now().UTC()
	m.affinityRuntime.now = func() time.Time { return now }
	observe := func(id, window string, period time.Duration, reset time.Time, amount float64) {
		t.Helper()
		lease, err := m.QuotaLease(id)
		if err != nil {
			t.Fatal(err)
		}
		scope, _ := quota.SharedCredential("generation")
		rolling, _ := quota.Rolling(period)
		remaining, _ := quota.Fraction(amount, quota.ComparisonKey{})
		s, err := quota.NewSnapshot(quota.SnapshotInput{Credential: lease.Credential(), Source: quota.ExistingUsageGET, ObservedAt: now, Windows: []quota.WindowInput{{ID: window, Scope: scope, Period: rolling, ResetAt: &reset, Remaining: remaining}}})
		if err != nil || !m.ObserveQuota(lease, s) {
			t.Fatalf("observation rejected: %v", err)
		}
	}
	servedSession := func(step, session string) string {
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
