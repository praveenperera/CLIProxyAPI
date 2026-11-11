package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	quota "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// An account-wide weekly window and a used-up Fable-only one, as Claude's usage reply gives them
func fableObservation(t *testing.T, lease QuotaLease, received, reset time.Time) quota.Snapshot {
	t.Helper()
	shared, _ := quota.SharedCredential("generation")
	fable, err := quota.Reported("generation", "Fable")
	if err != nil {
		t.Fatal(err)
	}
	week, _ := quota.Rolling(7 * 24 * time.Hour)
	weekly, _ := quota.Fraction(.6, quota.ComparisonKey{})
	empty, _ := quota.Fraction(0, quota.ComparisonKey{})
	s, err := quota.NewSnapshot(quota.SnapshotInput{Credential: lease.Credential(), Source: quota.ExistingUsageGET, ObservedAt: received, Windows: []quota.WindowInput{
		{ID: "seven_day", Scope: shared, Period: week, ResetAt: &reset, Remaining: weekly},
		{ID: "seven_day_fable", Scope: fable, Period: week, ResetAt: &reset, Remaining: empty},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQuotaHistoryKeepsTheFableLimitOutOfRouting(t *testing.T) {
	m, _, cfg, _, ids := affinityFixture(t)
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	m.affinityRuntime.now = func() time.Time { return now }
	lease, err := m.QuotaLease(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(3 * 24 * time.Hour)
	if !m.ObserveQuota(lease, fableObservation(t, lease, now, reset)) {
		t.Fatal("observation rejected")
	}

	labels := func(windows []QuotaHistoryWindow) map[string]string {
		out := make(map[string]string)
		for _, window := range windows {
			out[window.ID] = window.Model
		}
		return out
	}
	history := m.QuotaHistory(now.Add(-time.Hour), now)
	if got := labels(history.Credentials[0].Windows); len(got) != 2 || got["seven_day"] != "" || got["seven_day_fable"] != "Fable" {
		t.Fatalf("labels = %v", got)
	}

	// the recorded fallback for routing reads only the account-wide week, not the used-up Fable one
	recorded := m.affinityRuntime.history.weeklyQuota(ids[0], now, time.Hour)
	if !recorded.hasRemaining || recorded.remaining != .6 || !recorded.reset.Equal(reset) {
		t.Fatalf("weekly quota = %+v", recorded)
	}

	// the label survives a restart, so the fallback still skips the window
	if err = m.CloseQuotaAffinity(); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(nil, &FillFirstSelector{}, nil)
	if err = restarted.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := restarted.CloseQuotaAffinity(); errClose != nil {
			t.Error(errClose)
		}
	}()
	reloaded := restarted.affinityRuntime.history
	if got := labels(reloaded.query(now.Add(-time.Hour), now)[ids[0]].Windows); got["seven_day_fable"] != "Fable" || got["seven_day"] != "" {
		t.Fatalf("reloaded labels = %v", got)
	}
	if after := reloaded.weeklyQuota(ids[0], now, time.Hour); after.remaining != .6 {
		t.Fatalf("reloaded weekly quota = %+v", after)
	}
}

func TestQuotaHistoryWritesLabelledLinesThatOlderBinariesSkip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, affinity.QuotaHistoryFileName)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * 24 * time.Hour).Unix()
	// the second line is how the first build with model limits wrote a labelled window
	lines := fmt.Sprintf(`{"auth_id":"a","provider":"claude","window":"seven_day","period_seconds":604800,"t":%[1]d,"remaining":0.6,"reset_at":%[2]d}
{"auth_id":"a","provider":"claude","window":"seven_day_fable","model":"Fable","period_seconds":604800,"t":%[1]d,"remaining":0,"reset_at":%[2]d}
`, now.Unix(), reset)
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	h := openQuotaHistory(dir, func() time.Time { return now })
	if got := h.query(now.Add(-time.Hour), now)["a"].Windows; len(got) != 2 {
		t.Fatalf("windows = %+v", got)
	}
	if recorded := h.weeklyQuota("a", now, time.Hour); recorded.remaining != .6 {
		t.Fatalf("weekly quota = %+v", recorded)
	}
	h.close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := make(map[string]map[string]any)
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var fields map[string]any
		if err = json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		id, _ := fields["window"].(string)
		if reported, ok := fields["reported"].(string); ok {
			id = reported
		}
		rewritten[id] = fields
	}

	// binaries before model limits require window, so a used-up Fable line without one is skipped
	// rather than read as the account's weekly quota
	fable := rewritten["seven_day_fable"]
	if _, hasWindow := fable["window"]; hasWindow || fable["model"] != "Fable" {
		t.Fatalf("fable line = %v", fable)
	}
	if account := rewritten["seven_day"]; account["window"] != "seven_day" || account["reported"] != nil || account["model"] != nil {
		t.Fatalf("account line = %v", account)
	}
}

func TestQuotaHistoryLabelledWindowsLeaveRoutingSlotsFree(t *testing.T) {
	m, _, _, _, ids := affinityFixture(t)
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	m.affinityRuntime.now = func() time.Time { return now.Add(time.Hour) }
	lease, err := m.QuotaLease(ids[0])
	if err != nil {
		t.Fatal(err)
	}

	week, _ := quota.Rolling(7 * 24 * time.Hour)
	half, _ := quota.Fraction(.5, quota.ComparisonKey{})
	// more labels over time than the 32 routing slots hold
	for batch := range 5 {
		var windows []quota.WindowInput
		for i := range 8 {
			label := fmt.Sprintf("Model %d", batch*8+i)
			scope, errScope := quota.Reported("generation", label)
			if errScope != nil {
				t.Fatal(errScope)
			}
			windows = append(windows, quota.WindowInput{ID: fmt.Sprintf("seven_day_model_%d", batch*8+i), Scope: scope, Period: week, Remaining: half})
		}
		s, errSnapshot := quota.NewSnapshot(quota.SnapshotInput{Credential: lease.Credential(), Source: quota.ExistingUsageGET, ObservedAt: now.Add(time.Duration(batch) * time.Minute), Windows: windows})
		if errSnapshot != nil {
			t.Fatal(errSnapshot)
		}
		if !m.ObserveQuota(lease, s) {
			t.Fatalf("batch %d rejected", batch)
		}
	}

	later := now.Add(10 * time.Minute)
	if !m.ObserveQuota(lease, fableObservation(t, lease, later, later.Add(24*time.Hour))) {
		t.Fatal("account window rejected")
	}
	m.affinityMu.Lock()
	routed := m.affinityRuntime.observations[ids[0]].windows
	m.affinityMu.Unlock()
	if _, ok := routed["seven_day"]; !ok || len(routed) != 1 {
		t.Fatalf("routing windows = %v", routed)
	}
	if got := len(m.QuotaHistory(now.Add(-time.Hour), later).Credentials[0].Windows); got != 42 {
		t.Fatalf("history windows = %d", got)
	}
}
