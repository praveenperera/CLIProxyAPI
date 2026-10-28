package auth_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type transientPayloadTransport func(*http.Request) (*http.Response, error)

func (f transientPayloadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTransientRetryClaudePayloadRules(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			const model = "claude-opus-5-5"
			cfg := &config.Config{Payload: config.PayloadConfig{
				Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Match: []map[string]any{{"max_tokens": 64}}}}, Params: map[string]any{"max_tokens": 17}}},
				Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"temperature"}}},
			}}
			m := auth.NewTransientRetryTestManager()
			m.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
			id := "payload-" + t.Name()
			_, err := m.Register(context.Background(), &auth.Auth{ID: id, Provider: "claude", Attributes: map[string]string{"api_key": "fake-key", "base_url": "https://fake.invalid"}})
			if err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })

			var bodies [][]byte
			transport := transientPayloadTransport(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				if err := req.Body.Close(); err != nil {
					return nil, err
				}
				bodies = append(bodies, body)
				status, contentType := 529, "application/json"
				response := `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`
				if len(bodies) == 3 {
					status = 200
					response = `{"id":"msg_retry","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
					if stream {
						contentType = "text/event-stream"
						response = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_retry\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			payload := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"temperature":0.5,"messages":[{"role":"user","content":"hello"}]}`)
			req := exec.Request{Model: model, Payload: payload}
			opts := exec.Options{SourceFormat: translator.FormatClaude, OriginalRequest: payload}
			if stream {
				result, err := m.ExecuteStream(ctx, []string{"claude"}, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 3 {
				t.Fatalf("upstream attempts = %d, want 3", len(bodies))
			}
			for i, body := range bodies {
				if !bytes.Equal(body, bodies[0]) || gjson.GetBytes(body, "max_tokens").Int() != 17 || gjson.GetBytes(body, "temperature").Exists() {
					t.Fatalf("attempt %d changed final payload or lost rules: %s", i+1, body)
				}
			}
			if gjson.GetBytes(payload, "max_tokens").Int() != 64 || !gjson.GetBytes(payload, "temperature").Exists() {
				t.Fatalf("original request was changed: %s", payload)
			}
		})
	}
}
