package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestSessionBindingsSurviveRestart(t *testing.T) {
	m, model, ids, nowRef, observe := orderedFixture(t)
	cfg := &config.Config{Routing: config.RoutingConfig{QuotaAwareChatAffinity: &config.QuotaAffinityConfig{Namespace: "test", StateDir: m.affinityRuntime.settings.StateDir}}}
	week := 7 * 24 * time.Hour
	useOrder(m, config.RoutingOrderWeeklyReset)
	observe(ids[0], "seven_day", week, nowRef.Add(24*time.Hour), .5)
	observe(ids[1], "seven_day", week, nowRef.Add(3*24*time.Hour), .5)
	if got := servedBy(t, m, model, "before restart", "session-kept"); got != ids[0] {
		t.Fatalf("before restart served %s, want %s", got, ids[0])
	}
	if err := m.CloseQuotaAffinity(); err != nil {
		t.Fatal(err)
	}

	// the restarted process sees the other credential resetting sooner
	restarted := NewManager(nil, NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: NewOrderedSelector([]string{config.RoutingOrderWeeklyReset})}), nil)
	restarted.RegisterExecutor(&affinityTestExecutor{})
	for i, id := range ids {
		a := &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "fake-key-" + fmt.Sprint(i), "base_url": "https://fake.invalid"}}
		if _, err := restarted.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	}
	if err := restarted.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restarted.CloseQuotaAffinity(); err != nil {
			t.Error(err)
		}
	})
	restarted.SetConfig(cfg)
	restarted.affinityRuntime.now = func() time.Time { return *nowRef }
	quotaObserver(t, restarted, nowRef)(ids[1], "seven_day", week, nowRef.Add(time.Hour), .5)

	if got := servedBy(t, restarted, model, "after restart", "session-kept"); got != ids[0] {
		t.Fatalf("restart moved the bound session to %s, want %s", got, ids[0])
	}
	if got := servedBy(t, restarted, model, "new session", "session-new"); got != ids[1] {
		t.Fatalf("new session served %s, want the sooner-resetting %s", got, ids[1])
	}
}
