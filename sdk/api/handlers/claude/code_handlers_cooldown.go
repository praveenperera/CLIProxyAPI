package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// writeLongCooldown answers a long credential-pool cooldown with the wrapped
// gateway response and reports whether it wrote one
func (h *ClaudeCodeAPIHandler) writeLongCooldown(c *gin.Context, msg *interfaces.ErrorMessage) bool {
	cooldownStatus, cooldownBody, ok := h.claudeCodeLongCooldownResponse(c, msg)
	if !ok {
		return false
	}
	appendClaudeAPIResponse(c, cooldownBody)
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.Header().Set("X-Should-Retry", "false")
	c.Status(cooldownStatus)
	_, _ = c.Writer.Write(cooldownBody)
	return true
}

// claudeCodeMaxRetryAfter is the longest 429 Retry-After that Claude Code waits out
// on its own; above it the CLI fails the turn at once
const claudeCodeMaxRetryAfter = 60 * time.Second

// claudeCodeLongCooldownResponse rewrites a credential-pool cooldown that Claude Code
// would not retry into the wrapped gateway form `503 [CLIProxyAPI] [429]: <error>
// (reset after 1h2m3s)`. With token auth Claude Code ignores the unified rate-limit
// headers and reports only the status and message text, and T3 Code reads the reset
// time for auto-resume from exactly this form. X-Should-Retry stops Claude Code from
// retrying the 503. Only the local pool cooldown qualifies: Home concurrency and
// plugin retry hints are not usage limits. A response already committed by a
// keep-alive cannot change status, so it keeps the in-stream error
func (h *ClaudeCodeAPIHandler) claudeCodeLongCooldownResponse(c *gin.Context, msg *interfaces.ErrorMessage) (int, []byte, bool) {
	if msg == nil || msg.Error == nil || c == nil || c.Request == nil {
		return 0, nil, false
	}
	if !strings.HasPrefix(c.GetHeader("User-Agent"), "claude-cli") {
		return 0, nil, false
	}
	var cooldown interface{ IsModelCooldown() bool }
	if !errors.As(msg.Error, &cooldown) || !cooldown.IsModelCooldown() {
		return 0, nil, false
	}

	seconds, errAtoi := strconv.Atoi(coreauth.SafeResponseHeaders(msg.Error).Get("Retry-After"))
	resetIn := time.Duration(seconds) * time.Second
	if errAtoi != nil || resetIn <= claudeCodeMaxRetryAfter {
		return 0, nil, false
	}

	inner := h.toClaudeError(msg)
	inner.Error.Type = "rate_limit_error"
	innerBody, errMarshal := json.Marshal(inner)
	if errMarshal != nil {
		return 0, nil, false
	}

	body, errMarshal := json.Marshal(claudeErrorResponse{
		Type: "error",
		Error: claudeErrorDetail{
			Type:    "api_error",
			Message: fmt.Sprintf("[CLIProxyAPI] [429]: %s (reset after %s)", innerBody, resetIn),
		},
	})
	if errMarshal != nil {
		return 0, nil, false
	}
	return http.StatusServiceUnavailable, body, true
}
