package management

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// quotaHistoryDefaultRange is the chart range used when since is omitted
const quotaHistoryDefaultRange = 14 * 24 * time.Hour

type quotaHistoryResponse struct {
	Enabled          bool                          `json:"enabled"`
	RetentionSeconds int64                         `json:"retention_seconds"`
	Credentials      []quotaHistoryCredentialEntry `json:"credentials"`
}

type quotaHistoryCredentialEntry struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	// QuotaWeight sizes this credential against others of its provider when pooling
	QuotaWeight float64                   `json:"quota_weight"`
	Windows     []quotaHistoryWindowEntry `json:"windows"`
}

type quotaHistoryWindowEntry struct {
	ID            string                   `json:"id"`
	PeriodSeconds int64                    `json:"period_seconds"`
	Points        []quotaHistoryPointEntry `json:"points"`
}

type quotaHistoryPointEntry struct {
	T int64 `json:"t"`
	// Remaining is a percent from 0 to 100
	Remaining float64 `json:"remaining"`
	ResetAt   int64   `json:"reset_at"`
}

// GetQuotaHistory returns recorded remaining-quota points for the quota-aware chat affinity profile
// Query parameters since and until are optional unix seconds; the default range is the last 14 days
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	now := time.Now()
	since, okSince := quotaHistoryTimeParam(c, "since", now.Add(-quotaHistoryDefaultRange))
	until, okUntil := quotaHistoryTimeParam(c, "until", now)
	if !okSince || !okUntil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "since and until must be non-negative unix seconds"})
		return
	}
	if since.After(until) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "since must not be after until"})
		return
	}

	resp := quotaHistoryResponse{RetentionSeconds: int64(coreauth.QuotaHistoryRetention / time.Second), Credentials: []quotaHistoryCredentialEntry{}}
	if h.authManager == nil {
		c.JSON(http.StatusOK, resp)
		return
	}
	history := h.authManager.QuotaHistory(since, until)
	resp.Enabled = history.Enabled
	for _, credential := range history.Credentials {
		entry := quotaHistoryCredentialEntry{AuthID: credential.AuthID, AuthIndex: credential.AuthIndex, Provider: credential.Provider, QuotaWeight: credential.QuotaWeight, Windows: make([]quotaHistoryWindowEntry, 0, len(credential.Windows))}
		for _, window := range credential.Windows {
			windowEntry := quotaHistoryWindowEntry{ID: window.ID, PeriodSeconds: int64(window.Period / time.Second), Points: make([]quotaHistoryPointEntry, 0, len(window.Points))}
			for _, point := range window.Points {
				var resetAt int64
				if !point.ResetAt.IsZero() {
					resetAt = point.ResetAt.Unix()
				}
				// two decimals avoid float noise such as 83.50000000000001 in the chart
				remaining := math.Round(point.Remaining*10000) / 100
				windowEntry.Points = append(windowEntry.Points, quotaHistoryPointEntry{T: point.At.Unix(), Remaining: remaining, ResetAt: resetAt})
			}
			entry.Windows = append(entry.Windows, windowEntry)
		}
		resp.Credentials = append(resp.Credentials, entry)
	}
	c.JSON(http.StatusOK, resp)
}

func quotaHistoryTimeParam(c *gin.Context, name string, fallback time.Time) (time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, true
	}
	seconds, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil || seconds < 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}
