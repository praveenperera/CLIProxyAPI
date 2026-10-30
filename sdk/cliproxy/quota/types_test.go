package quota

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
)

var testNow = time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)

func ref(t *testing.T) affinity.CredentialRef {
	t.Helper()
	r, err := affinity.NewCredentialRef("codex", "auth", [32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func fraction(t *testing.T, value float64, key string) Remaining {
	t.Helper()
	var group ComparisonKey
	var err error
	if key != "" {
		group, err = NewComparisonKey(key)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := Fraction(value, group)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func amount(t *testing.T, value float64) Remaining {
	t.Helper()
	group, err := NewComparisonKey("codex/shared/token/verified-entitlement")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Amount(value, "token", group)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func window(t *testing.T, id string, reset time.Time, remaining Remaining) WindowInput {
	t.Helper()
	scope, err := SharedCredential("generation")
	if err != nil {
		t.Fatal(err)
	}
	var resetAt *time.Time
	if !reset.IsZero() {
		resetAt = &reset
	}
	return WindowInput{ID: id, Scope: scope, ResetAt: resetAt, Remaining: remaining}
}
func snapshot(t *testing.T, at time.Time, windows ...WindowInput) Snapshot {
	t.Helper()
	s, err := NewSnapshot(SnapshotInput{ref(t), PassiveHTTP, at, windows})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func candidate(t *testing.T, id string, priority int, weight float64, snapshot Snapshot) Candidate {
	t.Helper()
	c, err := NewCandidate(id, priority, weight, snapshot.View("model", testNow, 5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestValidationPresenceAndImmutability(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, 1.1} {
		if _, err := Fraction(bad, ComparisonKey{}); err == nil {
			t.Fatal("invalid fraction accepted")
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), -1} {
		if _, err := Amount(bad, "token", ComparisonKey{}); err == nil {
			t.Fatal("invalid amount accepted")
		}
	}
	if _, known := UnknownRemaining().Value(); known {
		t.Fatal("unknown became zero")
	}
	zero := fraction(t, 0, "")
	if value, known := zero.Value(); !known || value != 0 {
		t.Fatal("zero lost presence")
	}
	if _, err := ModelSet("meter", nil); err == nil {
		t.Fatal("empty model set accepted")
	}
	w := window(t, "primary", testNow.Add(time.Hour), fraction(t, 0.5, ""))
	s := snapshot(t, testNow, w)
	w.ResetAt = &testNow
	w.Remaining = zero
	view := s.View("model", testNow, 5*time.Minute)
	if view.Denied() {
		t.Fatal("input mutation changed snapshot")
	}
	copy := view.Windows()
	copy[0] = Window{}
	if len(view.Windows()) != 1 || view.Windows()[0].ID() != "primary" {
		t.Fatal("view not immutable")
	}
	if _, err := NewSnapshot(SnapshotInput{ref(t), PassiveHTTP, testNow, []WindowInput{w, w}}); err == nil {
		t.Fatal("duplicate window accepted")
	}
	if _, err := NewSnapshot(SnapshotInput{ref(t), Source(99), testNow, nil}); err == nil {
		t.Fatal("invalid source accepted")
	}
	if _, err := NewSnapshot(SnapshotInput{ref(t), PassiveHTTP, time.Time{}, nil}); err == nil {
		t.Fatal("missing receipt accepted")
	}
	if _, err := NewSnapshot(SnapshotInput{ref(t), PassiveHTTP, testNow, make([]WindowInput, 33)}); err == nil {
		t.Fatal("unbounded windows accepted")
	}
}
func TestViewFreshZeroStaleResetAndApplicability(t *testing.T) {
	zero := window(t, "weekly", testNow.Add(time.Hour), fraction(t, 0, ""))
	positive := window(t, "short", testNow.Add(2*time.Hour), fraction(t, 0.9, ""))
	fresh := snapshot(t, testNow, zero, positive).View("model", testNow, 5*time.Minute)
	if !fresh.Denied() {
		t.Fatal("positive companion overrides zero")
	}
	stale := snapshot(t, testNow.Add(-6*time.Minute), zero).View("model", testNow, 5*time.Minute)
	if stale.Denied() || stale.Freshness() != Stale || stale.Knowledge() != Unknown {
		t.Fatal("stale zero treated as current denial")
	}
	expired := snapshot(t, testNow, window(t, "expired", testNow, fraction(t, 0, ""))).View("model", testNow, 5*time.Minute)
	if expired.Denied() || expired.Knowledge() != Unknown {
		t.Fatal("passed reset refilled or denied")
	}
	denial := window(t, "denial", time.Time{}, UnknownRemaining())
	no := false
	denial.Allowed = &no
	if !snapshot(t, testNow, denial).View("model", testNow, 5*time.Minute).Denied() {
		t.Fatal("explicit denial without reset ignored")
	}
	scoped, err := ModelSet("review", []string{"review-model"})
	if err != nil {
		t.Fatal(err)
	}
	zero.Scope = scoped
	view := snapshot(t, testNow, zero, positive).View("model", testNow, 5*time.Minute)
	if view.Denied() || len(view.Windows()) != 1 {
		t.Fatal("unrelated meter constrained generation")
	}
	future := snapshot(t, testNow.Add(time.Second), positive).View("model", testNow, 5*time.Minute)
	if future.Freshness() != Stale {
		t.Fatal("future receipt accepted as fresh")
	}
}
func TestRankingEarliestWindowsPriorityAndUnknownFallback(t *testing.T) {
	next := testNow.Add(time.Hour)
	later := testNow.Add(2 * time.Hour)
	a := snapshot(t, testNow, window(t, "weekly-primary", next, fraction(t, 0.1, "")), window(t, "secondary", later, fraction(t, 0.9, "")))
	b := snapshot(t, testNow, window(t, "primary", later, fraction(t, 0.99, "")))
	unknown := snapshot(t, testNow, window(t, "unknown", time.Time{}, UnknownRemaining()))
	if result := Rank([]Candidate{candidate(t, "a", 1, 1, a), candidate(t, "b", 1, 1, b)}); result.Winner() != "a" {
		t.Fatal("did not choose earliest across windows")
	}
	if result := Rank([]Candidate{candidate(t, "a", 1, 1, a), candidate(t, "b", 2, 1, b)}); result.Winner() != "b" {
		t.Fatal("priority boundary lost")
	}
	if result := Rank([]Candidate{candidate(t, "a", 1, 0, a), candidate(t, "b", 1, 1, b)}); result.Winner() != "b" {
		t.Fatal("zero weight admitted")
	}
	if result := Rank([]Candidate{candidate(t, "a", 1, 1, a), candidate(t, "b", 2, 1, unknown)}); result.Winner() != "" || len(result.Fallback()) != 1 || result.Fallback()[0] != "b" {
		t.Fatal("unknown higher tier lost fallback")
	}
	partial := snapshot(t, testNow, window(t, "positive", next, fraction(t, 0.1, "")), window(t, "unknown", time.Time{}, UnknownRemaining()))
	if !partial.View("model", testNow, 5*time.Minute).Partial() || Rank([]Candidate{candidate(t, "z", 1, 1, partial), candidate(t, "a", 1, 1, unknown)}).Winner() != "z" {
		t.Fatal("partial did not rank")
	}
	denied := snapshot(t, testNow, window(t, "zero", next, fraction(t, 0, "")))
	if result := Rank([]Candidate{candidate(t, "a", 2, 1, denied), candidate(t, "b", 1, 1, b)}); result.Winner() != "b" {
		t.Fatal("denied unavailable tier blocked available tier")
	}
	if result := Rank([]Candidate{candidate(t, "a", 1, 1, denied)}); result.Winner() != "" || len(result.Fallback()) != 0 {
		t.Fatal("denied fell back")
	}
}
func TestRankingComparableBottleneckAndIncomparableGroups(t *testing.T) {
	next := testNow.Add(time.Hour)
	later := testNow.Add(2 * time.Hour)
	a := snapshot(t, testNow, window(t, "short", next, amount(t, 500)), window(t, "weekly", later, amount(t, 50)))
	b := snapshot(t, testNow, window(t, "short", next, amount(t, 100)), window(t, "weekly", later, amount(t, 80)))
	if result := Rank([]Candidate{candidate(t, "a", 1, 1, a), candidate(t, "b", 1, 1, b)}); result.Winner() != "b" {
		t.Fatal("absolute bottleneck not used")
	}
	a = snapshot(t, testNow, window(t, "primary", next, fraction(t, 0.1, "")))
	b = snapshot(t, testNow, window(t, "primary", next, fraction(t, 0.9, "")))
	if result := Rank([]Candidate{candidate(t, "b", 1, 1, b), candidate(t, "a", 1, 1, a)}); result.Winner() != "a" {
		t.Fatal("unverified fractions treated as capacity")
	}
	// group winners must be selected before comparing IDs across incomparable groups
	a = snapshot(t, testNow, window(t, "primary", next, fraction(t, 0.1, "verified-A")))
	b = snapshot(t, testNow, window(t, "primary", next, fraction(t, 0.9, "verified-A")))
	c := snapshot(t, testNow, window(t, "primary", next, fraction(t, 0.2, "verified-B")))
	for _, list := range [][]Candidate{{candidate(t, "a", 1, 1, a), candidate(t, "z", 1, 1, b), candidate(t, "b", 1, 1, c)}, {candidate(t, "b", 1, 1, c), candidate(t, "z", 1, 1, b), candidate(t, "a", 1, 1, a)}} {
		if result := Rank(list); result.Winner() != "b" {
			t.Fatal("non-transitive comparison")
		}
	}
}
