package quota

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
)

func TestCombinedKnowledgeKeepsUnsupportedDistinct(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ref, _ := affinity.NewCredentialRef("codex", "fake", [32]byte{1})
	scope, _ := SharedCredential("generation")
	snapshot := func(id string, remaining Remaining, received time.Time) Snapshot {
		t.Helper()
		s, err := NewSnapshot(SnapshotInput{Credential: ref, Source: PassiveHTTP, ObservedAt: received, Windows: []WindowInput{{ID: id, Scope: scope, Remaining: remaining}}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	unsupported := snapshot("unsupported", UnsupportedRemaining(), now)
	unknown := snapshot("unknown", UnknownRemaining(), now)
	knownRemaining, _ := Fraction(.5, ComparisonKey{})
	known := snapshot("known", knownRemaining, now)
	for _, test := range []struct {
		snapshots []Snapshot
		want      Knowledge
		freshness Freshness
	}{
		{[]Snapshot{unsupported}, Unsupported, Fresh},
		{[]Snapshot{unsupported, unknown}, Unknown, Fresh},
		{[]Snapshot{unsupported, known}, Known, Fresh},
		{[]Snapshot{snapshot("old", UnsupportedRemaining(), now.Add(-time.Hour))}, Unknown, Stale},
	} {
		v := Combine(test.snapshots, "model", now, 5*time.Minute)
		if v.Knowledge() != test.want || v.Freshness() != test.freshness {
			t.Fatalf("knowledge/freshness = %v/%v, want %v/%v", v.Knowledge(), v.Freshness(), test.want, test.freshness)
		}
	}
}
