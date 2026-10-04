// Package quota parses read-only provider observations at their receipt boundary
package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	domain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

const maxUsageBytes = 1 << 20

var errObservation = errors.New("quota observation unavailable")

// HTTPDoer is an authenticated host transport; provider credentials remain host-owned
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// FetchUsage reads only the inspected built-in OAuth usage GET endpoint
// The credential owner must validate lineage and generation again before ingestion
// No timeout, reserve operation, reset operation, or configurable URL is added
func FetchUsage(ctx context.Context, client HTTPDoer, ref affinity.CredentialRef, accountID string) (domain.Snapshot, error) {
	return fetchUsage(ctx, client, ref, accountID, time.Now)
}
func fetchUsage(ctx context.Context, client HTTPDoer, ref affinity.CredentialRef, accountID string, now func() time.Time) (domain.Snapshot, error) {
	if client == nil || ref.AuthID() == "" {
		return domain.Snapshot{}, errObservation
	}
	endpoint := ""
	switch ref.Provider() {
	case "claude":
		endpoint = "https://api.anthropic.com/api/oauth/usage"
	case "codex":
		if accountID == "" || len(accountID) > 256 {
			return domain.Snapshot{}, errObservation
		}
		if _, err := affinity.NewCallerScope(accountID); err != nil {
			return domain.Snapshot{}, errObservation
		}
		endpoint = "https://chatgpt.com/backend-api/wham/usage"
	default:
		return domain.Snapshot{}, errObservation
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return domain.Snapshot{}, errObservation
	}
	req.Header.Set("Accept", "application/json")
	if ref.Provider() == "claude" {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	} else {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	response, err := client.Do(req)
	// capture receipt before reading the body, not at stream or parsing completion
	received := now().UTC()
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			return domain.Snapshot{}, ctx.Err()
		}
		return domain.Snapshot{}, errObservation
	}
	if response == nil || response.Body == nil {
		return domain.Snapshot{}, errObservation
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, maxUsageBytes+1))
	errClose := response.Body.Close()
	if ctx.Err() != nil {
		return domain.Snapshot{}, ctx.Err()
	}
	if response.StatusCode != http.StatusOK || errRead != nil || errClose != nil || len(body) > maxUsageBytes {
		return domain.Snapshot{}, errObservation
	}
	if ref.Provider() == "claude" {
		return ParseClaudeUsage(ref, body, received)
	}
	return ParseCodexUsage(ref, body, received)
}

func decode(body []byte, target any) error {
	if len(body) == 0 || len(body) > maxUsageBytes {
		return errObservation
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(target) != nil {
		return errObservation
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errObservation
	}
	return nil
}
func resetUnix(value *int64) *time.Time {
	if value == nil || *value <= 0 || *value > 253402300799 {
		return nil
	}
	t := time.Unix(*value, 0).UTC()
	return &t
}
func remainingPercent(value *float64) domain.Remaining {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 100 {
		return domain.UnknownRemaining()
	}
	r, err := domain.Fraction(1-*value/100, domain.ComparisonKey{})
	if err != nil {
		return domain.UnknownRemaining()
	}
	return r
}
func snapshot(ref affinity.CredentialRef, source domain.Source, received time.Time, windows []domain.WindowInput) (domain.Snapshot, error) {
	s, err := domain.NewSnapshot(domain.SnapshotInput{Credential: ref, Source: source, ObservedAt: received, Windows: windows})
	if err != nil {
		return domain.Snapshot{}, errObservation
	}
	return s, nil
}
func sharedWindow(id, meter string, period domain.Period, reset *time.Time, remaining domain.Remaining, allowed *bool) domain.WindowInput {
	scope, _ := domain.SharedCredential(meter)
	return domain.WindowInput{ID: id, Scope: scope, Period: period, ResetAt: reset, Remaining: remaining, Allowed: allowed}
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// ParseClaudeUsage parses percentages from the existing OAuth usage payload
// Only inspected shared generation meters are applicable; labels do not invent scope
func ParseClaudeUsage(ref affinity.CredentialRef, body []byte, received time.Time) (domain.Snapshot, error) {
	if ref.Provider() != "claude" {
		return domain.Snapshot{}, errObservation
	}
	var payload map[string]json.RawMessage
	if err := decode(body, &payload); err != nil || payload == nil {
		return domain.Snapshot{}, errObservation
	}
	windows := make([]domain.WindowInput, 0, 2)
	for _, item := range []struct {
		name   string
		period time.Duration
	}{{"five_hour", 5 * time.Hour}, {"seven_day", 7 * 24 * time.Hour}} {
		raw, present := payload[item.name]
		if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var window claudeUsageWindow
		if json.Unmarshal(raw, &window) != nil {
			return domain.Snapshot{}, errObservation
		}
		var reset *time.Time
		if window.ResetsAt != nil {
			if parsed, err := time.Parse(time.RFC3339Nano, *window.ResetsAt); err == nil && parsed.Year() >= 1970 {
				reset = &parsed
			}
		}
		period, _ := domain.Rolling(item.period)
		windows = append(windows, sharedWindow(item.name, "generation", period, reset, remainingPercent(window.Utilization), nil))
	}
	return snapshot(ref, domain.ExistingUsageGET, received, windows)
}

type codexUsageWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowSeconds *int64   `json:"limit_window_seconds"`
	WindowMinutes *int64   `json:"window_minutes"`
	ResetAt       *int64   `json:"reset_at"`
	ResetAfter    *int64   `json:"reset_after_seconds"`
}
type codexLimits struct {
	Allowed         *bool             `json:"allowed"`
	LimitReached    *bool             `json:"limit_reached"`
	PrimaryWindow   *codexUsageWindow `json:"primary_window"`
	SecondaryWindow *codexUsageWindow `json:"secondary_window"`
	Primary         *codexUsageWindow `json:"primary"`
	Secondary       *codexUsageWindow `json:"secondary"`
}

func codexWindows(limits *codexLimits, received time.Time, event bool) []domain.WindowInput {
	if limits == nil {
		return nil
	}
	allowed := limits.Allowed
	if limits.LimitReached != nil && *limits.LimitReached {
		no := false
		allowed = &no
	}
	windows := make([]domain.WindowInput, 0, 3)
	if allowed != nil {
		windows = append(windows, sharedWindow("generation-allowed", "generation", domain.Period{}, nil, domain.UnknownRemaining(), allowed))
	}
	primary, secondary := limits.PrimaryWindow, limits.SecondaryWindow
	if event {
		primary, secondary = limits.Primary, limits.Secondary
	}
	for _, item := range []struct {
		id     string
		window *codexUsageWindow
	}{{"primary", primary}, {"secondary", secondary}} {
		if item.window == nil {
			continue
		}
		w := item.window
		period := domain.Period{}
		// duration comes from the payload; primary is not necessarily a five-hour period
		if !event && w.WindowSeconds != nil && *w.WindowSeconds > 0 && *w.WindowSeconds <= 366*24*3600 {
			period, _ = domain.Rolling(time.Duration(*w.WindowSeconds) * time.Second)
		}
		if event && w.WindowMinutes != nil && *w.WindowMinutes > 0 && *w.WindowMinutes <= 366*24*60 {
			period, _ = domain.Rolling(time.Duration(*w.WindowMinutes) * time.Minute)
		}
		reset := resetUnix(w.ResetAt)
		remaining := remainingPercent(w.UsedPercent)
		if w.ResetAfter != nil {
			if *w.ResetAfter < 0 || *w.ResetAfter > 366*24*3600 {
				reset = nil
				remaining = domain.UnknownRemaining()
			} else {
				relative := received.Add(time.Duration(*w.ResetAfter) * time.Second)
				if reset == nil {
					reset = &relative
				} else if delta := reset.Sub(relative); delta < -2*time.Second || delta > 2*time.Second {
					reset = nil
					remaining = domain.UnknownRemaining()
				}
			}
		}
		windows = append(windows, sharedWindow(item.id, "generation", period, reset, remaining, nil))
	}
	return windows
}

// ParseCodexUsage keeps null windows absent and excludes code-review/unmapped additional meters
func ParseCodexUsage(ref affinity.CredentialRef, body []byte, received time.Time) (domain.Snapshot, error) {
	if ref.Provider() != "codex" {
		return domain.Snapshot{}, errObservation
	}
	var payload struct {
		RateLimit *codexLimits `json:"rate_limit"`
	}
	if err := decode(body, &payload); err != nil {
		return domain.Snapshot{}, err
	}
	return snapshot(ref, domain.ExistingUsageGET, received, codexWindows(payload.RateLimit, received, false))
}

// ParseCodexEvent parses one complete generation event with its original receipt time
// Unknown named meters do not establish applicability to the generation route
func ParseCodexEvent(ref affinity.CredentialRef, body []byte, received time.Time) (domain.Snapshot, error) {
	if ref.Provider() != "codex" {
		return domain.Snapshot{}, errObservation
	}
	var payload struct {
		Type   string       `json:"type"`
		Meter  string       `json:"metered_limit_name"`
		Name   string       `json:"limit_name"`
		Limits *codexLimits `json:"rate_limits"`
	}
	if err := decode(body, &payload); err != nil || payload.Type != "codex.rate_limits" {
		return domain.Snapshot{}, errObservation
	}
	if (payload.Meter != "" && payload.Meter != "codex") || (payload.Name != "" && payload.Name != "codex") {
		return snapshot(ref, domain.PassiveWebsocket, received, nil)
	}
	return snapshot(ref, domain.PassiveWebsocket, received, codexWindows(payload.Limits, received, true))
}

// ParseHeaders parses bounded shared generation headers at the supplied receipt boundary
func ParseHeaders(ref affinity.CredentialRef, headers http.Header, received time.Time) (domain.Snapshot, error) {
	if len(headers) > 256 {
		return domain.Snapshot{}, errObservation
	}
	windows := make([]domain.WindowInput, 0, 2)
	switch ref.Provider() {
	case "claude":
		for _, item := range []struct {
			id     string
			period time.Duration
		}{{"5h", 5 * time.Hour}, {"7d", 7 * 24 * time.Hour}} {
			prefix := "Anthropic-Ratelimit-Unified-" + item.id + "-"
			usedRaw := headers.Get(prefix + "Utilization")
			resetRaw := headers.Get(prefix + "Reset")
			status := headers.Get(prefix + "Status")
			if usedRaw == "" && resetRaw == "" && status == "" {
				continue
			}
			if len(usedRaw) > 512 || len(resetRaw) > 512 || len(status) > 512 {
				return domain.Snapshot{}, errObservation
			}
			remaining := domain.UnknownRemaining()
			if used, err := strconv.ParseFloat(usedRaw, 64); err == nil && used >= 0 && used <= 1 {
				remaining, _ = domain.Fraction(1-used, domain.ComparisonKey{})
			}
			var reset *time.Time
			if seconds, err := strconv.ParseInt(resetRaw, 10, 64); err == nil {
				reset = resetUnix(&seconds)
			}
			var allowed *bool
			if status == "rejected" {
				no := false
				allowed = &no
			} else if status == "allowed" || status == "allowed_warning" {
				yes := true
				allowed = &yes
			}
			period, _ := domain.Rolling(item.period)
			windows = append(windows, sharedWindow(map[string]string{"5h": "five_hour", "7d": "seven_day"}[item.id], "generation", period, reset, remaining, allowed))
		}
	case "codex":
		limits := &codexLimits{}
		if value, err := strconv.ParseBool(headers.Get("X-Codex-Allowed")); err == nil {
			limits.Allowed = &value
		}
		if value, err := strconv.ParseBool(headers.Get("X-Codex-Limit-Reached")); err == nil {
			limits.LimitReached = &value
		}
		for _, item := range []struct {
			id     string
			target **codexUsageWindow
		}{{"Primary", &limits.Primary}, {"Secondary", &limits.Secondary}} {
			prefix := "X-Codex-" + item.id + "-"
			used := headers.Get(prefix + "Used-Percent")
			minutes := headers.Get(prefix + "Window-Minutes")
			reset := headers.Get(prefix + "Reset-At")
			after := headers.Get(prefix + "Reset-After-Seconds")
			if used == "" && minutes == "" && reset == "" && after == "" {
				continue
			}
			if len(used) > 512 || len(minutes) > 512 || len(reset) > 512 || len(after) > 512 {
				return domain.Snapshot{}, errObservation
			}
			w := &codexUsageWindow{}
			if v, err := strconv.ParseFloat(used, 64); err == nil {
				w.UsedPercent = &v
			}
			if v, err := strconv.ParseInt(minutes, 10, 64); err == nil {
				w.WindowMinutes = &v
			}
			if v, err := strconv.ParseInt(reset, 10, 64); err == nil {
				w.ResetAt = &v
			}
			if v, err := strconv.ParseInt(after, 10, 64); err == nil {
				w.ResetAfter = &v
			}
			*item.target = w
		}
		windows = codexWindows(limits, received, true)
	default:
		return domain.Snapshot{}, errObservation
	}
	return snapshot(ref, domain.PassiveHTTP, received, windows)
}
