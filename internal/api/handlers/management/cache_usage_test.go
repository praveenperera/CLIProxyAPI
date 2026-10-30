package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestGetCacheUsageReturnsHitRatioJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := coreauth.NewManager(nil, nil, nil)
	m.CacheUsagePlugin().HandleUsage(context.Background(), usage.Record{
		AuthID: "claude-a.json", Provider: "claude", SessionID: "claude:abc", RequestedAt: time.Now(),
		Detail: usage.Detail{TokenBreakdown: usage.NewIndependentTokenBreakdown(250, 750, 0, 10, 0, 1010)},
	})
	h := &Handler{authManager: m}
	router := gin.New()
	router.GET("/cache", h.GetCacheUsage)

	get := func(query string) map[string]any {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/cache"+query, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d %s", query, recorder.Code, recorder.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	body := get("?session_id=abc")
	totals := body["totals"].(map[string]any)
	if totals["hit_ratio"] != .75 || totals["cache_read_tokens"] != float64(750) {
		t.Fatalf("totals = %v", totals)
	}
	sessions := body["sessions"].([]any)
	last := sessions[0].(map[string]any)["last_request"].(map[string]any)
	if len(sessions) != 1 || last["hit_ratio"] != .75 {
		t.Fatalf("sessions = %v", sessions)
	}
	hours := body["credentials"].([]any)[0].(map[string]any)["hours"].([]any)
	if len(hours) != 1 || hours[0].(map[string]any)["t"] == nil || hours[0].(map[string]any)["requests"] != float64(1) {
		t.Fatalf("hours = %v", hours)
	}

	// a range with no requests reports a null ratio rather than zero
	if empty := get("?since=1&until=2")["totals"].(map[string]any); empty["hit_ratio"] != nil {
		t.Fatalf("empty totals = %v", empty)
	}
}
