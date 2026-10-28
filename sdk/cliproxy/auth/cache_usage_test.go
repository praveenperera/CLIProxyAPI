package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func cacheUsageRecord(authID, session string, at time.Time, uncached, read, write int64) usage.Record {
	return usage.Record{
		AuthID:      authID,
		Provider:    "claude",
		Model:       "claude-opus-5-5",
		SessionID:   session,
		RequestedAt: at,
		Detail:      usage.Detail{TokenBreakdown: usage.NewIndependentTokenBreakdown(uncached, read, write, 10, 0, uncached+read+write+10)},
	}
}

func TestCacheUsageReportsHitRatiosAndSurvivesRestart(t *testing.T) {
	m, _, cfg, _, ids := affinityFixture(t)
	now := time.Now()
	plugin := m.CacheUsagePlugin()
	// a cold start writes the cache, the next turn reads it
	plugin.HandleUsage(context.Background(), cacheUsageRecord(ids[0], "claude:s1", now.Add(-2*time.Minute), 50, 0, 950))
	plugin.HandleUsage(context.Background(), cacheUsageRecord(ids[0], "claude:s1", now.Add(-time.Minute), 100, 900, 0))
	plugin.HandleUsage(context.Background(), cacheUsageRecord(ids[1], "claude:s2", now, 1000, 0, 0))
	failed := cacheUsageRecord(ids[1], "claude:s2", now, 0, 5000, 0)
	failed.Failed = true
	plugin.HandleUsage(context.Background(), failed)

	query := CacheUsageQuery{Since: now.Add(-time.Hour), Until: now.Add(time.Minute)}
	report := m.CacheUsage(query)
	if ratio, _ := report.Totals.HitRatio(); report.Totals.InputTokens != 3000 || report.Totals.Requests != 3 || ratio != .3 {
		t.Fatalf("totals = %+v ratio %v, want 3 requests, 3000 input, ratio .3 without the failed request", report.Totals, ratio)
	}
	if len(report.Credentials) != 2 || report.Credentials[0].AuthID != ids[0] || report.Credentials[0].Totals.CacheWriteTokens != 950 {
		t.Fatalf("credentials = %+v", report.Credentials)
	}

	// agents look up their own session by its bare ID
	query.SessionID = "s1"
	sessions := m.CacheUsage(query).Sessions
	if len(sessions) != 1 || sessions[0].Totals.Requests != 2 || sessions[0].LastRequest.CacheReadTokens != 900 {
		t.Fatalf("session s1 = %+v", sessions)
	}

	m.cacheUsage.close()
	restarted := NewManager(nil, nil, nil)
	restarted.cacheUsage.persistTo(cfg.Routing.QuotaAwareChatAffinity.StateDir)
	again := restarted.CacheUsage(CacheUsageQuery{Since: query.Since, Until: query.Until})
	if !again.Persisted || again.Totals != report.Totals || len(again.Sessions) != 2 {
		t.Fatalf("after restart totals = %+v sessions %d, want %+v and 2", again.Totals, len(again.Sessions), report.Totals)
	}
}

func TestCacheUsageSessionEdgeCases(t *testing.T) {
	dir := t.TempDir()
	hour := time.Now().Truncate(time.Hour)
	m := NewManager(nil, nil, nil)
	plugin := m.CacheUsagePlugin()
	ctx := context.Background()

	// requests finish out of order; the latest started one names the session's credential
	plugin.HandleUsage(ctx, cacheUsageRecord("b.json", "claude:s1", hour.Add(20*time.Minute), 0, 1000, 0))
	plugin.HandleUsage(ctx, cacheUsageRecord("a.json", "claude:s1", hour.Add(10*time.Minute), 1000, 0, 0))
	// two requests started within one second still order by start time
	plugin.HandleUsage(ctx, cacheUsageRecord("b.json", "claude:s3", hour.Add(800*time.Millisecond), 0, 1000, 0))
	plugin.HandleUsage(ctx, cacheUsageRecord("a.json", "claude:s3", hour.Add(100*time.Millisecond), 1000, 0, 0))
	// incomplete accounting is not counted
	partial := cacheUsageRecord("a.json", "claude:s1", hour.Add(30*time.Minute), 0, 0, 0)
	partial.Detail.TokenBreakdown = usage.NewPartialSubsetTokenBreakdown(100, 0, 0, 0, 0, 140)
	plugin.HandleUsage(ctx, partial)
	// the same bare id under two providers lists both
	plugin.HandleUsage(ctx, cacheUsageRecord("c.json", "codex:s1", hour.Add(15*time.Minute), 10, 0, 0))

	query := CacheUsageQuery{Since: hour.Add(15 * time.Minute), Until: hour.Add(16 * time.Minute), SessionID: "s1"}
	report := m.CacheUsage(query)
	if !report.Since.Equal(hour) || !report.Until.Equal(hour.Add(time.Hour)) {
		t.Fatalf("bounds = %v..%v, want the whole hour from %v", report.Since, report.Until, hour)
	}
	if len(report.Sessions) != 2 {
		t.Fatalf("bare id matched %d sessions, want claude and codex", len(report.Sessions))
	}
	if same := m.CacheUsage(CacheUsageQuery{Since: hour, Until: hour, SessionID: "claude:s3"}).Sessions; len(same) != 1 || same[0].AuthID != "b.json" {
		t.Fatalf("same-second session = %+v, want the later-started b.json request", same)
	}

	// an exact id wins over the same id under a provider prefix
	plugin.HandleUsage(ctx, cacheUsageRecord("a.json", "s2", hour.Add(15*time.Minute), 10, 0, 0))
	plugin.HandleUsage(ctx, cacheUsageRecord("a.json", "claude:s2", hour.Add(15*time.Minute), 10, 0, 0))
	query.SessionID = "s2"
	if exact := m.CacheUsage(query).Sessions; len(exact) != 1 || exact[0].SessionID != "s2" {
		t.Fatalf("exact id s2 matched %+v", exact)
	}
	query.SessionID = "claude:s1"
	sessions := m.CacheUsage(query).Sessions
	if len(sessions) != 1 {
		t.Fatalf("exact id matched %d sessions", len(sessions))
	}
	s := sessions[0]
	if s.AuthID != "b.json" || s.LastRequest.CacheReadTokens != 1000 || s.Totals.Requests != 2 || !s.FirstSeen.Equal(hour.Add(10*time.Minute)) {
		t.Fatalf("session = %+v, want latest b.json warm request, 2 requests, first seen at :10", s)
	}

	// a snapshot loaded after requests already arrived adds to them instead of being dropped
	m.cacheUsage.persistTo(dir)
	m.cacheUsage.close()
	restarted := NewManager(nil, nil, nil)
	restarted.CacheUsagePlugin().HandleUsage(ctx, cacheUsageRecord("b.json", "claude:s1", hour.Add(40*time.Minute), 0, 500, 0))
	restarted.cacheUsage.persistTo(dir)
	merged := restarted.CacheUsage(query).Sessions
	if len(merged) != 1 || merged[0].Totals.Requests != 3 || merged[0].LastRequest.CacheReadTokens != 500 {
		t.Fatalf("merged session = %+v, want 3 requests with the newest last request", merged)
	}
}
