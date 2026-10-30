package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// An overload reported as the first SSE event reaches the conductor as a stream error with no
// payload, so it can retry before the client receives anything
func TestClaudeExecutorStreamLeadingOverloadIsStreamError(t *testing.T) {
	events := map[string]string{
		"single line": "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
		"split data":  "event: error\ndata: {\"type\":\"error\",\ndata: \"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
	}
	for name, event := range events {
		t.Run(name, func(t *testing.T) { assertLeadingOverloadIsStreamError(t, event) })
	}
}

func assertLeadingOverloadIsStreamError(t *testing.T, event string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, event)
	}))
	defer server.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123", "base_url": server.URL}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var chunkErr error
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			chunkErr = chunk.Err
			continue
		}
		output.Write(chunk.Payload)
	}
	if output.Len() != 0 {
		t.Fatalf("leading overload was forwarded: %q", output.String())
	}
	if chunkErr == nil || !strings.Contains(chunkErr.Error(), "overloaded_error") {
		t.Fatalf("chunk error = %v, want the overload as a stream error", chunkErr)
	}
}
