package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	quotadomain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

func getQuotaHistory(t *testing.T, h *Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/quota/history"+query, nil)
	h.GetQuotaHistory(ctx)
	return rec
}

func TestGetQuotaHistoryShapeAndDisabled(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	disabled := NewHandlerWithoutConfigFilePath(&config.Config{}, coreauth.NewManager(nil, nil, nil))
	rec := getQuotaHistory(t, disabled, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"enabled":false,"retention_seconds":2592000,"credentials":[]}` {
		t.Fatalf("disabled response %d %s", rec.Code, rec.Body.String())
	}

	manager := coreauth.NewManager(nil, nil, nil)
	cfg := &config.Config{Routing: config.RoutingConfig{QuotaAwareChatAffinity: &config.QuotaAffinityConfig{Namespace: "test", StateDir: filepath.Join(t.TempDir(), "bindings")}}}
	if errConfigure := manager.ConfigureQuotaAffinity(cfg); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	t.Cleanup(func() {
		if errClose := manager.CloseQuotaAffinity(); errClose != nil {
			t.Error(errClose)
		}
	})
	auth := &coreauth.Auth{ID: "history-auth", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "fake-key", "base_url": "https://fake.invalid", "plan_type": "prolite"}, Metadata: map[string]any{"email": " user@example.com "}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	lease, errLease := manager.QuotaLease(auth.ID)
	if errLease != nil {
		t.Fatal(errLease)
	}
	observed := time.Now().Add(-time.Minute).Truncate(time.Second)
	reset := observed.Add(48 * time.Hour)
	scope, _ := quotadomain.SharedCredential("generation")
	period, _ := quotadomain.Rolling(7 * 24 * time.Hour)
	remaining, _ := quotadomain.Fraction(.835, quotadomain.ComparisonKey{})
	snapshot, errSnapshot := quotadomain.NewSnapshot(quotadomain.SnapshotInput{Credential: lease.Credential(), Source: quotadomain.ExistingUsageGET, ObservedAt: observed, Windows: []quotadomain.WindowInput{{ID: "seven_day", Scope: scope, Period: period, ResetAt: &reset, Remaining: remaining}}})
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	if !manager.ObserveQuota(lease, snapshot) {
		t.Fatal("observation rejected")
	}

	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	rec = getQuotaHistory(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var payload quotaHistoryResponse
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatal(errDecode)
	}
	want := quotaHistoryResponse{Enabled: true, RetentionSeconds: 2592000, Credentials: []quotaHistoryCredentialEntry{{
		AuthID: "history-auth", AuthIndex: auth.EnsureIndex(), Provider: "codex", QuotaWeight: 1, Email: "user@example.com", PlanType: "prolite",
		Windows: []quotaHistoryWindowEntry{{ID: "seven_day", PeriodSeconds: 604800, Points: []quotaHistoryPointEntry{{T: observed.Unix(), Remaining: 83.5, ResetAt: reset.Unix()}}}},
	}}}
	got, _ := json.Marshal(payload)
	expected, _ := json.Marshal(want)
	if string(got) != string(expected) {
		t.Fatalf("payload %s, want %s", got, expected)
	}

	rec = getQuotaHistory(t, h, "?since="+strconv.FormatInt(observed.Unix()+1, 10))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"credentials":[]`) {
		t.Fatalf("range after the point: %d %s", rec.Code, rec.Body.String())
	}
	for _, query := range []string{"?since=abc", "?until=-1", "?since=200&until=100"} {
		if rec = getQuotaHistory(t, h, query); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status %d, want 400", query, rec.Code)
		}
	}
}
