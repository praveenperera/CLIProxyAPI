package claude

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// t3CodeGatewayLimitPattern mirrors the result text T3 Code's Claude adapter parses
// for a reset time, after Claude Code prefixes the status as `API Error: 503 `
var t3CodeGatewayLimitPattern = regexp.MustCompile(`^API Error: 503 \[[^\]\r\n]+\] \[429\]: \{"type":"error","error":\{"type":"rate_limit_error",[^\r\n]*\} \(reset after (?:(\d+)h ?)?(?:(\d+)m ?)?(?:(\d+)s)?\)`)

func TestWriteClaudeErrorResponse_LongCooldownForClaudeCodeCarriesResetTime(t *testing.T) {
	tests := []struct {
		name       string
		userAgent  string
		err        error
		wantStatus int
	}{
		{"claude code long cooldown", "claude-cli/2.1.311 (external, sdk-cli)", coreauth.NewModelCooldownError("claude-opus-5-5", "claude", 2*time.Hour+3*time.Minute+4*time.Second), http.StatusServiceUnavailable},
		{"claude code short cooldown it retries itself", "claude-cli/2.1.311 (external, sdk-cli)", coreauth.NewModelCooldownError("claude-opus-5-5", "claude", time.Minute), http.StatusTooManyRequests},
		{"other client long cooldown", "anthropic-sdk-python/1.0", coreauth.NewModelCooldownError("claude-opus-5-5", "claude", 2*time.Hour), http.StatusTooManyRequests},
		{"claude code home concurrency busy is not a usage limit", "claude-cli/2.1.311 (external, sdk-cli)", coreauth.NewHomeConcurrencyBusyError("busy", 2*time.Minute), http.StatusTooManyRequests},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("User-Agent", tt.userAgent)

			(&ClaudeCodeAPIHandler{}).WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: tt.err})

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantStatus != http.StatusServiceUnavailable {
				return
			}
			if got := recorder.Header().Get("X-Should-Retry"); got != "false" {
				t.Fatalf("X-Should-Retry = %q, want false", got)
			}
			message := ojson.GetBytes(recorder.Body.Bytes(), "error.message").String()
			match := t3CodeGatewayLimitPattern.FindStringSubmatch("API Error: 503 " + message)
			if match == nil || match[1] != "2" || match[2] != "3" || match[3] != "4" {
				t.Fatalf("message %q does not carry reset 2h3m4s in the T3 Code form", message)
			}
			if !strings.Contains(message, "All credentials for model claude-opus-5-5 are cooling down via provider claude") {
				t.Fatalf("message %q lost the cooldown detail", message)
			}
		})
	}
}
