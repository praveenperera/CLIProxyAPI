package auth

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestCacheUsagePriceKey(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-5-5":     "claude-opus-5-5",
		"claude-opus-5":       "claude-opus-5",
		"Claude-Opus-5-5[1m]": "claude-opus-5-5",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": "claude-sonnet-4-5",
		"claude-sonnet-4-5@20250929":                   "claude-sonnet-4-5",
		"gpt-5.5-codex":                                "gpt-5.5-codex",
	} {
		if got := cacheUsagePriceKey(model); got != want {
			t.Errorf("cacheUsagePriceKey(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestCacheUsageCostByModelAndTier(t *testing.T) {
	m, _, cfg, _, ids := affinityFixture(t)
	dir := cfg.Routing.QuotaAwareChatAffinity.StateDir
	hour := time.Now().Truncate(time.Hour)
	at := hour.Add(time.Minute)

	// totals saved before cost tracking load as unpriced
	m.cacheUsage.close()
	legacy := `{"version":1,"hours":[{"auth_id":"` + ids[0] + `","provider":"claude","t":` + strconv.FormatInt(hour.Unix(), 10) +
		`,"totals":{"requests":2,"input_tokens":100,"uncached_tokens":100,"output_tokens":4}}]}`
	if errWrite := os.WriteFile(filepath.Join(dir, affinity.CacheUsageFileName), []byte(legacy), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	m = NewManager(nil, nil, nil)
	m.cacheUsage.persistTo(dir)
	// write the final snapshot before the fixture removes the state directory
	t.Cleanup(m.cacheUsage.close)

	record := func(model, tier string, uncached, read, write, write1h int64) usage.Record {
		r := cacheUsageRecord(ids[0], "", at, uncached, read, write)
		r.Model = "alias-the-client-asked-for"
		r.ResponseModel, r.ResponseServiceTier = model, tier
		r.Detail.CacheWrite1hTokens = write1h
		return r
	}
	plugin := m.CacheUsagePlugin()
	// 600 of the 1000 written tokens went to the 1-hour cache
	plugin.HandleUsage(context.Background(), record("claude-opus-5-5", "", 100, 900, 1000, 600))
	plugin.HandleUsage(context.Background(), record("claude-opus-5-5-20261001", "fast", 100, 900, 1000, 600))
	// Haiku 5.5 prices a prompt over 100K tokens higher, cache reads included
	plugin.HandleUsage(context.Background(), record("claude-haiku-5-5", "", 0, 150_000, 0, 0))
	plugin.HandleUsage(context.Background(), record("claude-haiku-5-5", "", 1000, 0, 0, 0))
	plugin.HandleUsage(context.Background(), record("some-unpriced-model", "", 1000, 0, 0, 0))

	report := m.CacheUsage(CacheUsageQuery{Since: hour, Until: at})
	opus := (100*4 + 400*5 + 600*8 + 900*0.2 + 10*20) / 1e6
	haiku := (150_000*0.05 + 10*2.5 + 1000*0.1 + 10*0.5) / 1e6
	want := map[string]struct {
		cost, noCache float64
		unpriced      int64
	}{
		"claude-opus-5-5":     {3 * opus, 3 * (2000*4 + 10*20) / 1e6, 0},
		"claude-haiku-5-5":    {haiku, (150_000*0.5 + 10*2.5 + 1000*0.1 + 10*0.5) / 1e6, 0},
		"some-unpriced-model": {0, 0, 1},
		"":                    {0, 0, 2},
	}
	if len(report.Models) != len(want) {
		t.Fatalf("models = %+v", report.Models)
	}
	for _, model := range report.Models {
		w := want[model.Model]
		if math.Abs(model.Totals.CostUSD-w.cost) > 1e-12 || math.Abs(model.Totals.NoCacheCostUSD-w.noCache) > 1e-12 || model.Totals.UnpricedRequests != w.unpriced {
			t.Errorf("%q: cost %v no-cache %v unpriced %d, want %+v", model.Model, model.Totals.CostUSD, model.Totals.NoCacheCostUSD, model.Totals.UnpricedRequests, w)
		}
	}

	credential := report.Credentials[0]
	if len(credential.Hours) != 1 || credential.Hours[0].Totals.Requests != 7 || credential.Totals.UnpricedRequests != 3 {
		t.Fatalf("one credential hour should merge every model: %+v", credential)
	}
}
