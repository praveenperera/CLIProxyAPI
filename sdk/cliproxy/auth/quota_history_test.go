package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	quota "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// historyObservation pairs a fractional window with amount and unknown windows that history must skip
func historyObservation(t *testing.T, lease QuotaLease, received, reset time.Time, fraction float64) quota.Snapshot {
	t.Helper()
	scope, _ := quota.SharedCredential("generation")
	period, _ := quota.Rolling(5 * time.Hour)
	remaining, _ := quota.Fraction(fraction, quota.ComparisonKey{})
	amount, _ := quota.Amount(10, "credits", quota.ComparisonKey{})
	allowed := true
	s, err := quota.NewSnapshot(quota.SnapshotInput{Credential: lease.Credential(), Source: quota.ExistingUsageGET, ObservedAt: received, Windows: []quota.WindowInput{
		{ID: "primary", Scope: scope, Period: period, ResetAt: &reset, Remaining: remaining},
		{ID: "credits", Scope: scope, Remaining: amount},
		{ID: "generation-allowed", Scope: scope, Allowed: &allowed},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func historyFileLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("history file mode %v", info.Mode().Perm())
	}
	return bytes.Count(data, []byte("\n"))
}

func TestQuotaHistoryRecordsDownsamplesPersistsAndPrunes(t *testing.T) {
	m, _, cfg, _, ids := affinityFixture(t)
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	now := base
	m.affinityRuntime.now = func() time.Time { return now }
	lease, err := m.QuotaLease(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	reset := base.Add(5 * time.Hour)
	steps := []struct {
		offset time.Duration
		value  float64
		reset  time.Time
	}{
		{0, .80, reset}, // first point
		{time.Minute, .8004, reset.Add(30 * time.Second)}, // below delta, reset jitter: skipped
		{2 * time.Minute, .79, reset},                     // value change
		{10 * time.Minute, .79, reset},                    // unchanged before heartbeat: skipped
		{20 * time.Minute, .79, reset},                    // heartbeat
		{21 * time.Minute, .79, reset.Add(5 * time.Hour)}, // window rollover
	}
	for _, step := range steps {
		now = base.Add(step.offset)
		if !m.ObserveQuota(lease, historyObservation(t, lease, now, step.reset, step.value)) {
			t.Fatalf("observation at %v rejected", step.offset)
		}
	}

	history := m.QuotaHistory(base.Add(-time.Hour), now)
	auth, _ := m.GetByID(ids[0])
	if !history.Enabled || len(history.Credentials) != 1 {
		t.Fatalf("history = %+v", history)
	}
	credential := history.Credentials[0]
	if credential.AuthID != ids[0] || credential.Provider != "codex" || credential.AuthIndex == "" || credential.AuthIndex != auth.EnsureIndex() || credential.QuotaWeight != 1 {
		t.Fatalf("credential = %+v", credential)
	}

	// a larger plan's auth file sets quota_weight so pooled views count it for more
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	auth.Metadata["quota_weight"] = float64(2)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if weighted := m.QuotaHistory(base.Add(-time.Hour), now); weighted.Credentials[0].QuotaWeight != 2 {
		t.Fatalf("weighted credential = %+v", weighted.Credentials[0])
	}
	// a management field patch stores the number undecoded
	auth.Metadata["quota_weight"] = json.Number("1.7")
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if patched := m.QuotaHistory(base.Add(-time.Hour), now); patched.Credentials[0].QuotaWeight != 1.7 {
		t.Fatalf("patched credential = %+v", patched.Credentials[0])
	}
	if len(credential.Windows) != 1 || credential.Windows[0].ID != "primary" || credential.Windows[0].Period != 5*time.Hour {
		t.Fatalf("windows = %+v", credential.Windows)
	}
	var got []time.Duration
	for _, p := range credential.Windows[0].Points {
		got = append(got, p.At.Sub(base))
	}
	want := []time.Duration{0, 2 * time.Minute, 20 * time.Minute, 21 * time.Minute}
	if len(got) != len(want) {
		t.Fatalf("points at %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("points at %v, want %v", got, want)
		}
	}
	last := credential.Windows[0].Points[3]
	if last.Remaining != .79 || !last.ResetAt.Equal(reset.Add(5*time.Hour)) {
		t.Fatalf("last point = %+v", last)
	}
	if ranged := m.QuotaHistory(base.Add(time.Minute), base.Add(20*time.Minute)); len(ranged.Credentials[0].Windows[0].Points) != 2 {
		t.Fatalf("range filter = %+v", ranged.Credentials[0].Windows[0].Points)
	}

	if err = m.CloseQuotaAffinity(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Routing.QuotaAwareChatAffinity.StateDir, affinity.QuotaHistoryFileName)
	if lines := historyFileLines(t, path); lines != 4 {
		t.Fatalf("persisted %d lines, want 4", lines)
	}

	// the affinity store must reopen with the history log beside its bindings
	restarted := NewManager(nil, &FillFirstSelector{}, nil)
	if err = restarted.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	reloaded := restarted.affinityRuntime.history.query(base.Add(-time.Hour), now)
	if points := reloaded[ids[0]].Windows; len(points) != 1 || len(points[0].Points) != 4 || points[0].Period != 5*time.Hour {
		t.Fatalf("reloaded = %+v", reloaded)
	}
	// polling prunes bindings in the same directory; the history log is not a foreign record
	if err = restarted.affinityRuntime.store.Prune(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = restarted.CloseQuotaAffinity(); err != nil {
		t.Fatal(err)
	}

	// 30 days after base+10m, the first two points are past retention and compaction drops them
	later := base.Add(QuotaHistoryRetention + 10*time.Minute)
	pruned := openQuotaHistory(cfg.Routing.QuotaAwareChatAffinity.StateDir, func() time.Time { return later })
	kept := pruned.query(base, later)
	pruned.close()
	if windows := kept[ids[0]].Windows; len(windows) != 1 || len(windows[0].Points) != 2 || !windows[0].Points[0].At.Equal(base.Add(20*time.Minute)) {
		t.Fatalf("pruned = %+v", windows)
	}
	if lines := historyFileLines(t, path); lines != 2 {
		t.Fatalf("compacted %d lines, want 2", lines)
	}
}

func TestQuotaHistoryKeepsUnreadableLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, affinity.QuotaHistoryFileName)
	original := []byte("{\"auth_id\":\"a\",\"provider\":\"claude\",\"window\":\"seven_day\",\"period_seconds\":604800,\"t\":1,\"remaining\":0.5}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	// an unreadable log fails the load; rename over it would still succeed without the guard
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	h := openQuotaHistory(dir, func() time.Time { return now })
	now = now.Add(2 * quotaHistoryCompactEvery)
	h.maybeCompact()
	h.close()

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("unreadable log was rewritten: %q", data)
	}
}
