package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// A zero weight pauses an enabled credential: no strategy routes to it, not only
// weighted round-robin, so pausing an account is not limited to one routing setup
func TestZeroWeightPausesRoutingUnderEveryStrategy(t *testing.T) {
	selectors := map[string]func() Selector{
		"round-robin":          func() Selector { return &RoundRobinSelector{} },
		"fill-first":           func() Selector { return &FillFirstSelector{} },
		"ordered":              func() Selector { return NewOrderedSelector([]string{"priority"}) },
		"weighted-round-robin": func() Selector { return &WeightedRoundRobinSelector{} },
	}
	for name, newSelector := range selectors {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			model := "pause-" + name
			manager := NewManager(nil, newSelector(), nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "gemini"})

			// the paused credential has the higher priority, so priority-first strategies pick it unless weight excludes it
			paused := &Auth{ID: "paused-" + name, Provider: "gemini", Attributes: map[string]string{AttributeWeight: "0", "priority": "10"}}
			active := &Auth{ID: "active-" + name, Provider: "gemini"}
			for _, a := range []*Auth{paused, active} {
				if _, errRegister := manager.Register(ctx, a); errRegister != nil {
					t.Fatalf("register %s: %v", a.ID, errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, "gemini", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			}

			for index := 0; index < 6; index++ {
				got, _, errPick := manager.pickNext(ctx, "gemini", model, cliproxyexecutor.Options{}, nil)
				if errPick != nil || got == nil || got.ID != active.ID {
					t.Fatalf("pickNext() #%d = %v, %v; want %s", index, got, errPick, active.ID)
				}
				got, _, errLegacy := manager.pickNextLegacy(ctx, "gemini", model, cliproxyexecutor.Options{}, nil)
				if errLegacy != nil || got == nil || got.ID != active.ID {
					t.Fatalf("pickNextLegacy() #%d = %v, %v; want %s", index, got, errLegacy, active.ID)
				}
				got, _, _, errMixed := manager.pickNextMixed(ctx, []string{"gemini"}, model, cliproxyexecutor.Options{}, nil)
				if errMixed != nil || got == nil || got.ID != active.ID {
					t.Fatalf("pickNextMixed() #%d = %v, %v; want %s", index, got, errMixed, active.ID)
				}
			}

			// pinning the paused credential must not bypass the pause
			pinned := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: paused.ID}}
			if got, _, errPinned := manager.pickNext(ctx, "gemini", model, pinned, nil); errPinned == nil {
				t.Fatalf("pinned pickNext() = %s, want an error for a paused credential", got.ID)
			}

			tried := map[string]struct{}{active.ID: {}}
			if got, _, errOnlyPaused := manager.pickNext(ctx, "gemini", model, cliproxyexecutor.Options{}, tried); errOnlyPaused == nil {
				t.Fatalf("pickNext() with only the paused credential left = %s, want an error", got.ID)
			}
		})
	}
}

// Provider-level routers skip a provider whose only credential is paused, as they do for a
// disabled one, so they fall back instead of failing on a provider with nothing to route to
func TestPausedOnlyProviderIsNotAvailable(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	paused := &Auth{ID: "paused-only", Provider: "gemini", Metadata: map[string]any{AttributeWeight: 0}}
	if _, errRegister := manager.Register(ctx, paused); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	if manager.HasProviderAuth("gemini") || len(manager.AvailableProviders()) != 0 {
		t.Fatalf("paused-only provider reported available: has=%v providers=%v", manager.HasProviderAuth("gemini"), manager.AvailableProviders())
	}

	resumed := paused.Clone()
	delete(resumed.Metadata, AttributeWeight)
	if _, errUpdate := manager.Update(ctx, resumed); errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}
	if !manager.HasProviderAuth("gemini") {
		t.Fatal("resumed credential not reported available")
	}
}

// Pausing by weight must keep usage readings flowing, unlike disabling, so dashboards that
// read the quota history keep showing the paused account
func TestQuotaPollReadsZeroWeightCredential(t *testing.T) {
	m, e, _, _, ids := affinityFixture(t)
	for _, id := range ids {
		a, _ := m.GetByID(id)
		a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
		a.Metadata = map[string]any{"account_id": id, "oauth_provenance": testProvenance(t, id, "fake"), "access_token": "fake"}
		if id == ids[0] {
			a.Metadata[AttributeWeight] = 0
		}
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	polled := make(map[string]int)
	e.usage = func(_ context.Context, a *Auth, _ *http.Request) (*http.Response, error) {
		mu.Lock()
		polled[a.ID]++
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":3600,"reset_after_seconds":3600}}}`))}, nil
	}
	m.PollQuotaOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if polled[ids[0]] != 1 || polled[ids[1]] != 1 {
		t.Fatalf("polled = %v, want one read for the paused and the active credential", polled)
	}
}
