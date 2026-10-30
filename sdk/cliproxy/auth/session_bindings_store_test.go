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

func TestSessionBindingsRestoreLateCacheAndOrdering(t *testing.T) {
	dir := t.TempDir()
	saved := NewSessionCacheWithCapacity(time.Hour, 3)
	t.Cleanup(saved.Stop)
	saved.setSessionAliases("auth-c", "claude", "other", "other")
	// saved later, so its sequence outranks the live selection unless restore renumbers
	saved.setSessionAliases("auth-a", "claude", "parent", "parent::m1")
	writer := newSessionBindingsStore(func() *SessionCache { return saved })
	writer.open(dir)
	writer.close()

	// an SDK manager installs its session selector after the profile opens
	var live *SessionCache
	reader := newSessionBindingsStore(func() *SessionCache { return live })
	reader.open(dir)
	live = NewSessionCacheWithCapacity(time.Hour, 3)
	t.Cleanup(live.Stop)
	live.setSessionAliases("auth-b", "claude", "parent", "parent::m2")
	reader.save()

	if got, ok := live.Get("parent::m1"); !ok || got != "auth-a" {
		t.Fatalf("late cache restored parent::m1 = %q, %v; want auth-a", got, ok)
	}
	// the selection made before the restore is newer than every saved one
	if got := live.latestSessionAuth("parent", map[string]struct{}{"claude": {}}); got != "auth-b" {
		t.Fatalf("latest parent selection = %s, want the live auth-b", got)
	}
	// saved groups evict before live ones, oldest saved first
	live.setSessionAliases("auth-d", "claude", "new", "new")
	if _, ok := live.Get("other"); ok {
		t.Fatal("oldest saved group survived eviction")
	}
	for _, key := range []string{"parent::m1", "parent::m2", "new"} {
		if _, ok := live.Get(key); !ok {
			t.Fatalf("%s was evicted before older saved groups", key)
		}
	}

	// a replaced cache saves even when its change count matches the last write
	reader.close()
	reopened := newSessionBindingsStore(func() *SessionCache { return live })
	reopened.open(dir)
	reopened.save()
	replacement := NewSessionCacheWithCapacity(time.Hour, 3)
	t.Cleanup(replacement.Stop)
	reopened.cache = func() *SessionCache { return replacement }
	replacement.setSessionAliases("auth-e", "claude", "fresh", "fresh")
	replacement.changes = reopened.saved
	reopened.close()
	check := NewSessionCacheWithCapacity(time.Hour, 3)
	t.Cleanup(check.Stop)
	final := newSessionBindingsStore(func() *SessionCache { return check })
	final.open(dir)
	t.Cleanup(final.close)
	if got, ok := check.Get("fresh"); !ok || got != "auth-e" {
		t.Fatalf("replacement cache binding = %q, %v; want it saved", got, ok)
	}
}
