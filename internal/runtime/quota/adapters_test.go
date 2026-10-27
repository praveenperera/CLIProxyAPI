package quota

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	domain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

var now = time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)

func credential(t *testing.T, provider string) affinity.CredentialRef {
	t.Helper()
	ref, err := affinity.NewCredentialRef(provider, "auth", [32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func TestClaudePercentAndPassiveFractionAreDistinct(t *testing.T) {
	ref := credential(t, "claude")
	usage, err := ParseClaudeUsage(ref, []byte(`{"five_hour":{"utilization":25,"resets_at":"2026-10-03T18:00:00Z"},"seven_day":null}`), now)
	if err != nil {
		t.Fatal(err)
	}
	view := usage.View("claude-sonnet-5-5", now, 5*time.Minute)
	value, known := view.Windows()[0].Remaining().Value()
	if !known || value != 0.75 {
		t.Fatal("active percentage units wrong")
	}
	passive, err := ParseHeaders(ref, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.25"}, "Anthropic-Ratelimit-Unified-5h-Reset": []string{"1791050400"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	value, known = passive.View("model", now, 5*time.Minute).Windows()[0].Remaining().Value()
	if !known || value != 0.75 {
		t.Fatal("passive fraction units wrong")
	}
	bad, err := ParseHeaders(ref, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"25"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if bad.View("model", now, 5*time.Minute).Knowledge() != domain.Unknown {
		t.Fatal("percentage parsed as passive fraction")
	}
	empty, err := ParseClaudeUsage(ref, []byte(`{"five_hour":{"resets_at":"2026-10-03T18:00:00Z"}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if empty.View("model", now, 5*time.Minute).Denied() || empty.View("model", now, 5*time.Minute).Knowledge() != domain.Unknown {
		t.Fatal("missing utilization became zero")
	}
}
func TestCodexNullWeeklyPrimaryAndUnknownAdditionalMeters(t *testing.T) {
	ref := credential(t, "codex")
	payload := []byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1791050400},"secondary_window":null},"code_review_rate_limit":{"allowed":false},"additional_rate_limits":[{"limit_name":"unmapped","rate_limit":{"allowed":false}}]}`)
	s, err := ParseCodexUsage(ref, payload, now)
	if err != nil {
		t.Fatal(err)
	}
	view := s.View("model", now, 5*time.Minute)
	if view.Denied() {
		t.Fatal("unrelated meters denied generation")
	}
	windows := view.Windows()
	if len(windows) != 2 || windows[1].Period().Duration() != 7*24*time.Hour {
		t.Fatal("weekly primary guessed as five hours")
	}
	if value, known := windows[1].Remaining().Value(); !known || value != 0.8 {
		t.Fatal("percent parsed wrong")
	}
	if windows[1].Remaining().Comparison() != (domain.ComparisonKey{}) {
		t.Fatal("unverified denominator established")
	}
	missing, err := ParseCodexUsage(ref, []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"reset_at":1791050400}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if missing.View("model", now, 5*time.Minute).Windows()[0].Period().Duration() != 0 {
		t.Fatal("missing period invented")
	}
	zero, err := ParseCodexUsage(ref, []byte(`{"rate_limit":{"primary_window":{"used_percent":100,"reset_at":1791050400}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if !zero.View("model", now, 5*time.Minute).Denied() {
		t.Fatal("measured zero ignored")
	}
}
func TestCodexEventsReceiptAndResetConsistency(t *testing.T) {
	ref := credential(t, "codex")
	s, err := ParseCodexEvent(ref, []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":25,"window_minutes":10080,"reset_after_seconds":3600}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source() != domain.PassiveWebsocket || !s.ObservedAt().Equal(now) {
		t.Fatal("event receipt lost")
	}
	view := s.View("model", now, 5*time.Minute)
	reset, present := view.NextReset()
	if !present || !reset.Equal(now.Add(time.Hour)) {
		t.Fatal("relative reset wrong")
	}
	inconsistent, err := ParseCodexEvent(ref, []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":25,"window_minutes":300,"reset_at":1791050400,"reset_after_seconds":7200}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ranked := inconsistent.View("model", now, 5*time.Minute).NextReset(); ranked {
		t.Fatal("inconsistent reset ranked")
	}
	scoped, err := ParseCodexEvent(ref, []byte(`{"type":"codex.rate_limits","metered_limit_name":"unknown-meter","rate_limits":{"allowed":false}}`), now)
	if err != nil || scoped.View("model", now, 5*time.Minute).Denied() {
		t.Fatal("unmapped meter treated as shared", err)
	}
	if _, err := ParseCodexEvent(ref, []byte(`{"type":"response.completed"}`), now); err == nil {
		t.Fatal("non-quota event accepted")
	}
	if s.View("model", now.Add(6*time.Minute), 5*time.Minute).Freshness() != domain.Stale {
		t.Fatal("receipt refreshed")
	}
}

type doFunc func(*http.Request) (*http.Response, error)

func (f doFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

type advancingBody struct {
	io.Reader
	advance func()
}

func (b *advancingBody) Read(data []byte) (int, error) { b.advance(); return b.Reader.Read(data) }
func (b *advancingBody) Close() error                  { return nil }
func TestFetchUsesOnlyExactReadOnlyPathsAndHeaderReceipt(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			clock := now
			calls := 0
			client := doFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodGet || req.Body != nil {
					t.Fatal("non-read-only request")
				}
				expected := "https://api.anthropic.com/api/oauth/usage"
				payload := `{"five_hour":{"utilization":25,"resets_at":"2026-10-03T18:00:00Z"}}`
				if provider == "codex" {
					expected = "https://chatgpt.com/backend-api/wham/usage"
					payload = `{"rate_limit":{"primary_window":{"used_percent":25,"reset_at":1791050400}}}`
					if req.Header.Get("Chatgpt-Account-Id") != "actual-account" {
						t.Fatal("account ID missing")
					}
				} else if req.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
					t.Fatal("OAuth beta missing")
				}
				if req.URL.String() != expected {
					t.Fatal("unexpected endpoint", req.URL)
				}
				body := &advancingBody{strings.NewReader(payload), func() { clock = now.Add(10 * time.Minute) }}
				return &http.Response{StatusCode: 200, Body: body}, nil
			})
			s, err := fetchUsage(context.Background(), client, credential(t, provider), "actual-account", func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !s.ObservedAt().Equal(now) || s.View("model", clock, 5*time.Minute).Freshness() != domain.Stale {
				t.Fatal("receipt captured at body completion")
			}
		})
	}
}
func TestFetchFailureCancellationAndSanitizedErrors(t *testing.T) {
	ref := credential(t, "claude")
	for _, status := range []int{401, 403, 429, 500} {
		client := doFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("secret token account body"))}, nil
		})
		if _, err := FetchUsage(context.Background(), client, ref, ""); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsanitized failure")
		}
	}
	client := doFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("credential secret in transport error")
	})
	if _, err := FetchUsage(context.Background(), client, ref, ""); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("transport credential leak")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client = doFunc(func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() })
	if _, err := FetchUsage(ctx, client, ref, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	for _, body := range []string{`{"five_hour":{"utilization":101}}`, `{"five_hour":{"utilization":null}}`} {
		s, err := ParseClaudeUsage(ref, []byte(body), now)
		if err != nil {
			t.Fatal(err)
		}
		view := s.View("model", now, 5*time.Minute)
		if view.Denied() || view.Knowledge() != domain.Unknown {
			t.Fatal("invalid or null became zero")
		}
	}
	if _, err := ParseClaudeUsage(ref, []byte(`{"five_hour":{"utilization": "token secret"}}`), now); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("parser error leak")
	}
}

func TestFetchRateLimitCarriesRetryAfter(t *testing.T) {
	ref := credential(t, "claude")
	for header, want := range map[string]time.Duration{"120": 2 * time.Minute, "": 0, "garbage": 0, "-5": 0} {
		client := doFunc(func(*http.Request) (*http.Response, error) {
			response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("secret"))}
			if header != "" {
				response.Header.Set("Retry-After", header)
			}
			return response, nil
		})
		_, err := FetchUsage(context.Background(), client, ref, "")
		var limited *RateLimitedError
		if !errors.As(err, &limited) || limited.RetryAfter != want || strings.Contains(err.Error(), "secret") {
			t.Fatalf("Retry-After %q: %v", header, err)
		}
	}
}
