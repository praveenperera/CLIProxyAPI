package management

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// cacheUsageDefaultRange is the report range used when since is omitted
const cacheUsageDefaultRange = 24 * time.Hour

type cacheUsageResponse struct {
	Since            int64                       `json:"since"`
	Until            int64                       `json:"until"`
	Persisted        bool                        `json:"persisted"`
	RetentionSeconds int64                       `json:"retention_seconds"`
	Totals           cacheUsageTotalsEntry       `json:"totals"`
	Credentials      []cacheUsageCredentialEntry `json:"credentials"`
	Models           []cacheUsageModelEntry      `json:"models"`
	Sessions         []cacheUsageSessionEntry    `json:"sessions"`
}

type cacheUsageModelEntry struct {
	Model    string                `json:"model"`
	Provider string                `json:"provider"`
	Totals   cacheUsageTotalsEntry `json:"totals"`
}

type cacheUsageTotalsEntry struct {
	coreauth.CacheUsageTotals
	// HitRatio is cache read tokens over input tokens from 0 to 1, null without input
	HitRatio *float64 `json:"hit_ratio"`
}

type cacheUsageCredentialEntry struct {
	AuthID    string                `json:"auth_id"`
	AuthIndex string                `json:"auth_index,omitempty"`
	Provider  string                `json:"provider"`
	Email     string                `json:"email,omitempty"`
	PlanType  string                `json:"plan_type,omitempty"`
	Totals    cacheUsageTotalsEntry `json:"totals"`
	Hours     []cacheUsageHourEntry `json:"hours"`
}

type cacheUsageHourEntry struct {
	T int64 `json:"t"`
	cacheUsageTotalsEntry
}

type cacheUsageSessionEntry struct {
	SessionID       string                `json:"session_id"`
	ParentSessionID string                `json:"parent_session_id,omitempty"`
	AuthID          string                `json:"auth_id"`
	Provider        string                `json:"provider"`
	Model           string                `json:"model"`
	Title           string                `json:"title,omitempty"`
	Project         string                `json:"project,omitempty"`
	FirstSeen       int64                 `json:"first_seen"`
	LastSeen        int64                 `json:"last_seen"`
	Totals          cacheUsageTotalsEntry `json:"totals"`
	LastRequest     cacheUsageTotalsEntry `json:"last_request"`
}

func cacheUsageTotals(t coreauth.CacheUsageTotals) cacheUsageTotalsEntry {
	entry := cacheUsageTotalsEntry{CacheUsageTotals: t}
	// a millionth of a dollar is below any cost worth showing and drops float noise
	entry.CostUSD = math.Round(t.CostUSD*1e6) / 1e6
	entry.NoCacheCostUSD = math.Round(t.NoCacheCostUSD*1e6) / 1e6
	if ratio, ok := t.HitRatio(); ok {
		// four decimals keep the ratio readable without float noise
		rounded := math.Round(ratio*10000) / 10000
		entry.HitRatio = &rounded
	}
	return entry
}

// GetCacheUsage returns prompt cache usage by credential, hour, and client session
// Query parameters: since and until are optional unix seconds (default the last 24 hours) and
// widen to whole hours, reported back as since and until; auth_id limits the report to one
// credential; session_id limits sessions to one client session
func (h *Handler) GetCacheUsage(c *gin.Context) {
	now := time.Now()
	since, okSince := quotaHistoryTimeParam(c, "since", now.Add(-cacheUsageDefaultRange))
	until, okUntil := quotaHistoryTimeParam(c, "until", now)
	if !okSince || !okUntil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "since and until must be non-negative unix seconds"})
		return
	}
	if since.After(until) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "since must not be after until"})
		return
	}

	resp := cacheUsageResponse{
		Since:            since.Unix(),
		Until:            until.Unix(),
		RetentionSeconds: int64(coreauth.CacheUsageRetention / time.Second),
		Totals:           cacheUsageTotals(coreauth.CacheUsageTotals{}),
		Credentials:      []cacheUsageCredentialEntry{},
		Models:           []cacheUsageModelEntry{},
		Sessions:         []cacheUsageSessionEntry{},
	}
	if h.authManager == nil {
		c.JSON(http.StatusOK, resp)
		return
	}
	report := h.authManager.CacheUsage(coreauth.CacheUsageQuery{
		Since:     since,
		Until:     until,
		AuthID:    strings.TrimSpace(c.Query("auth_id")),
		SessionID: strings.TrimSpace(c.Query("session_id")),
	})
	resp.Since, resp.Until = report.Since.Unix(), report.Until.Unix()
	resp.Persisted = report.Persisted
	resp.Totals = cacheUsageTotals(report.Totals)
	for _, credential := range report.Credentials {
		entry := cacheUsageCredentialEntry{AuthID: credential.AuthID, AuthIndex: credential.AuthIndex, Provider: credential.Provider, Email: credential.Email, PlanType: credential.PlanType, Totals: cacheUsageTotals(credential.Totals), Hours: make([]cacheUsageHourEntry, 0, len(credential.Hours))}
		for _, hour := range credential.Hours {
			entry.Hours = append(entry.Hours, cacheUsageHourEntry{T: hour.Start.Unix(), cacheUsageTotalsEntry: cacheUsageTotals(hour.Totals)})
		}
		resp.Credentials = append(resp.Credentials, entry)
	}
	for _, model := range report.Models {
		resp.Models = append(resp.Models, cacheUsageModelEntry{Model: model.Model, Provider: model.Provider, Totals: cacheUsageTotals(model.Totals)})
	}
	for _, s := range report.Sessions {
		resp.Sessions = append(resp.Sessions, cacheUsageSessionEntry{SessionID: s.SessionID, ParentSessionID: s.ParentSessionID, AuthID: s.AuthID, Provider: s.Provider, Model: s.Model, Title: s.Title, Project: s.Project, FirstSeen: s.FirstSeen.Unix(), LastSeen: s.LastSeen.Unix(), Totals: cacheUsageTotals(s.Totals), LastRequest: cacheUsageTotals(s.LastRequest)})
	}
	c.JSON(http.StatusOK, resp)
}
