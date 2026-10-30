package helps

import (
	"bytes"

	"github.com/MoeForge/moego/ojson"
)

// ClaudeLeadingOverloadGuard turns an overload reported before any event reached the client
// into a stream error instead, so the conductor can retry the request before anything is sent
type ClaudeLeadingOverloadGuard struct {
	forwarded bool
}

// Drop reports whether the SSE event is a leading overload that must not be forwarded;
// upstreamStreamErr is the terminal error already observed for this event
func (g *ClaudeLeadingOverloadGuard) Drop(rawEvent []byte, upstreamStreamErr error) bool {
	return !g.forwarded && upstreamStreamErr != nil && isClaudeOverloadedSSEEvent(rawEvent)
}

// Sent records that an event reached the client, after which an overload is forwarded as is
func (g *ClaudeLeadingOverloadGuard) Sent() {
	g.forwarded = true
}

// isClaudeOverloadedSSEEvent reports an SSE error event whose error type is overloaded_error;
// data lines are joined first because SSE allows one JSON payload to span several of them
func isClaudeOverloadedSSEEvent(rawEvent []byte) bool {
	var dataLines [][]byte
	for _, line := range bytes.Split(rawEvent, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			dataLines = append(dataLines, bytes.TrimSpace(trimmed[len("data:"):]))
		}
	}
	payload := ojson.ParseBytes(bytes.Join(dataLines, []byte("\n")))
	return payload.Get("type").String() == "error" && payload.Get("error.type").String() == "overloaded_error"
}
