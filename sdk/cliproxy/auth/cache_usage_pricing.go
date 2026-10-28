package auth

import (
	"regexp"
	"strings"
)

// cacheUsagePrice is a model's first-party API list price in USD per million tokens
type cacheUsagePrice struct {
	Input, Write5m, Write1h, Read, Output float64
}

// cacheUsageHaikuLongPromptTokens is the prompt length above which Claude Haiku 5.5 bills a
// request at its long-prompt prices; the length counts cache reads and writes too
const cacheUsageHaikuLongPromptTokens = 100_000

// cacheUsageFastMultiplier scales a fast-mode request, which Anthropic bills at twice the
// standard rate on the models that offer it
const cacheUsageFastMultiplier = 2

// cacheUsagePrices holds Anthropic's list prices from
// https://platform.claude.com/docs/en/about-claude/pricing as of 2026-10-28; cost figures are
// what traffic would cost on the API, not what subscription credentials are billed
var cacheUsagePrices = map[string]cacheUsagePrice{
	"claude-fable-5-1":  {Input: 10, Write5m: 12.5, Write1h: 20, Read: 0.25, Output: 50},
	"claude-mythos-5-1": {Input: 10, Write5m: 12.5, Write1h: 20, Read: 0.25, Output: 50},
	"claude-fable-5":    {Input: 10, Write5m: 12.5, Write1h: 20, Read: 1, Output: 50},
	"claude-mythos-5":   {Input: 10, Write5m: 12.5, Write1h: 20, Read: 1, Output: 50},
	"claude-opus-5-5":   {Input: 4, Write5m: 5, Write1h: 8, Read: 0.2, Output: 20},
	"claude-opus-5":     {Input: 5, Write5m: 6.25, Write1h: 10, Read: 0.5, Output: 25},
	"claude-opus-4-8":   {Input: 5, Write5m: 6.25, Write1h: 10, Read: 0.5, Output: 25},
	"claude-opus-4-7":   {Input: 5, Write5m: 6.25, Write1h: 10, Read: 0.5, Output: 25},
	"claude-opus-4-6":   {Input: 5, Write5m: 6.25, Write1h: 10, Read: 0.5, Output: 25},
	"claude-opus-4-5":   {Input: 5, Write5m: 6.25, Write1h: 10, Read: 0.5, Output: 25},
	"claude-sonnet-5-5": {Input: 2, Write5m: 2.5, Write1h: 4, Read: 0.1, Output: 10},
	"claude-sonnet-5":   {Input: 2, Write5m: 2.5, Write1h: 4, Read: 0.2, Output: 10},
	"claude-sonnet-4-6": {Input: 3, Write5m: 3.75, Write1h: 6, Read: 0.3, Output: 15},
	"claude-sonnet-4-5": {Input: 3, Write5m: 3.75, Write1h: 6, Read: 0.3, Output: 15},
	"claude-haiku-5-5":  {Input: 0.1, Write5m: 0.125, Write1h: 0.2, Read: 0.01, Output: 0.5},
	"claude-haiku-4-5":  {Input: 1, Write5m: 1.25, Write1h: 2, Read: 0.1, Output: 5},
}

// cacheUsageLongPromptPrices replaces a model's prices for requests whose prompt is over
// cacheUsageHaikuLongPromptTokens
var cacheUsageLongPromptPrices = map[string]cacheUsagePrice{
	"claude-haiku-5-5": {Input: 0.5, Write5m: 0.625, Write1h: 1, Read: 0.05, Output: 2.5},
}

// cacheUsageFastModels bill fast-mode responses at cacheUsageFastMultiplier
var cacheUsageFastModels = map[string]bool{"claude-opus-5-5": true, "claude-opus-5": true, "claude-opus-4-8": true}

// cacheUsageModelSuffix matches what can follow a model ID: a dated snapshot, a Bedrock
// version, or a context or thinking marker such as "[1m]" or "(high)"
var cacheUsageModelSuffix = regexp.MustCompile(`(-\d{8})?(-v\d+(:\d+)?)?([\[(@].*)?$`)

// cacheUsagePriceKey reduces a served model name to its priced ID, dropping cloud prefixes
// such as "us.anthropic." and the suffixes in cacheUsageModelSuffix
func cacheUsagePriceKey(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(model, "claude-"); i > 0 {
		model = model[i:]
	}
	return cacheUsageModelSuffix.ReplaceAllString(model, "")
}

// cacheUsageCost prices one request at API list prices and what it would have cost without
// prompt caching, with every input token at the base input rate; ok is false for models
// without a known price
func cacheUsageCost(model, serviceTier string, t CacheUsageTotals) (cost, noCache float64, ok bool) {
	key := cacheUsagePriceKey(model)
	price, ok := cacheUsagePrices[key]
	if !ok {
		return 0, 0, false
	}
	if long, has := cacheUsageLongPromptPrices[key]; has && t.InputTokens > cacheUsageHaikuLongPromptTokens {
		price = long
	}
	write1h := min(t.CacheWrite1hTokens, t.CacheWriteTokens)
	cost = (float64(t.UncachedTokens)*price.Input +
		float64(t.CacheWriteTokens-write1h)*price.Write5m +
		float64(write1h)*price.Write1h +
		float64(t.CacheReadTokens)*price.Read +
		float64(t.OutputTokens)*price.Output) / 1e6
	noCache = (float64(t.InputTokens)*price.Input + float64(t.OutputTokens)*price.Output) / 1e6
	if strings.EqualFold(serviceTier, "fast") && cacheUsageFastModels[key] {
		cost, noCache = cost*cacheUsageFastMultiplier, noCache*cacheUsageFastMultiplier
	}
	return cost, noCache, true
}
