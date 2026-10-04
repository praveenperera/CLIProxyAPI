package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const nativeHistoryFixture = `{"model":"claude-sonnet-5-5","max_tokens":16000,"system":[{"type":"text","text":"stable caller system","cache_control":{"type":"ephemeral","ttl":"5m"}}],"tools":[{"name":"mcp__caller_tool","input_schema":{"type":"object","properties":{"x":{"type":"integer"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"first user"}]},{"role":"system","content":"keep this here"},{"role":"assistant","content":[{"type":"thinking","thinking":"opaque\nthought","signature":"opaque-short-=/+\u0000"},{"type":"redacted_thinking","data":"opaque-redacted-=/+\u0000"},{"type":"tool_use","id":"call","name":"mcp__caller_tool","input":{"x":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"unchanged result"}]}],"thinking":{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"error"}},"betas":["thinking-binding-controls-2026-08-01","future-caller-beta"],"metadata":{"user_id":"actual-caller-value"}}`
const nativeTransformations = `[{"type":"thinking_dropped","path":"messages.2.content.0","reason":"model_binding_mismatch"},{"type":"thinking_dropped","path":"messages.2.content.1","reason":"future_reason"},null,{"type":"thinking_dropped","reason":7}]`

func nativeTestResponse(stream bool) string {
	content := gjson.Get(nativeHistoryFixture, "messages.2.content").Raw
	if !stream {
		return `{"id":"msg_fake","type":"message","model":"claude-sonnet-5-5","content":` + content + `,"future_field":{"opaque":"keep"},"input_transformations":` + nativeTransformations + `,"usage":{"input_tokens":1,"output_tokens":1}}`
	}
	response := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_fake","model":"claude-sonnet-5-5","future_field":{"opaque":"keep"},"input_transformations":` + nativeTransformations + `,"usage":{"input_tokens":1}}}` + "\n\n"
	for i, block := range gjson.Parse(content).Array() {
		response += fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i, block.Raw, i)
	}
	return response + "event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn","future_delta":"keep"},"input_transformations":` + nativeTransformations + `,"usage":{"output_tokens":1}}` + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func nativeReturnedContent(response []byte, stream bool) []byte {
	if !stream {
		return []byte(gjson.GetBytes(response, "content").Raw)
	}
	var blocks []string
	for _, line := range bytes.Split(response, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(payload, "type").String() == "content_block_start" {
			blocks = append(blocks, gjson.GetBytes(payload, "content_block").Raw)
		}
	}
	return []byte("[" + strings.Join(blocks, ",") + "]")
}

func TestClaudeNativeHistoryWireAllMethods(t *testing.T) {
	for _, method := range []string{"execute", "stream", "count"} {
		t.Run(method, func(t *testing.T) {
			var captured [][]byte
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				captured = append(captured, body)
				if r.Header.Get("Authorization") != "Bearer sk-ant-oat-fake-native" || r.Header.Get("X-App") != "" || r.Header.Get("X-Stainless-Lang") != "" || r.Header.Get("X-Claude-Code-Session-Id") != "" {
					t.Fatalf("unexpected identity/auth headers: %v", r.Header)
				}
				for _, beta := range []string{"thinking-binding-controls-2026-08-01", "future-caller-beta", "caller-header-beta"} {
					if !strings.Contains(r.Header.Get("Anthropic-Beta"), beta) {
						t.Fatalf("lost beta %s", beta)
					}
				}
				response := nativeTestResponse(method == "stream")
				if method == "count" {
					if r.URL.Path != "/v1/messages/count_tokens" {
						t.Fatal(r.URL)
					}
					response = `{"input_tokens":9,"future_count":"keep","input_transformations":` + nativeTransformations + `}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat-fake-native", "base_url": "https://fake-native.invalid"}}
			e := NewClaudeExecutor(&config.Config{})
			opts := exec.Options{SourceFormat: translator.FormatClaude, NativeHistory: exec.NativeHistoryPreserve, Headers: http.Header{"Anthropic-Beta": {"caller-header-beta"}}}
			var returnedContent []byte
			for turn, model := range []string{"claude-sonnet-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-sonnet-5-5"} {
				payload := []byte(nativeHistoryFixture)
				if turn == 0 {
					messages := gjson.GetBytes(payload, "messages").Array()
					payload, _ = sjson.SetRawBytes(payload, "messages", []byte("["+messages[0].Raw+","+messages[1].Raw+"]"))
				}
				if turn > 0 {
					payload, _ = sjson.SetRawBytes(payload, "messages.-1", []byte(`{"role":"user","content":"second user"}`))
				}
				if turn > 0 && len(returnedContent) > 0 {
					payload, _ = sjson.SetRawBytes(payload, "messages.2.content", returnedContent)
				}
				req := exec.Request{Model: model, Payload: payload}
				var response []byte
				switch method {
				case "execute":
					got, err := e.Execute(ctx, a, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					response = got.Payload
				case "count":
					got, err := e.CountTokens(ctx, a, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					response = got.Payload
				case "stream":
					got, err := e.ExecuteStream(ctx, a, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range got.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						response = append(response, chunk.Payload...)
					}
				}
				if method != "count" {
					returnedContent = nativeReturnedContent(response, method == "stream")
					if string(returnedContent) != gjson.Get(nativeHistoryFixture, "messages.2.content").Raw {
						t.Fatalf("opaque response content changed: %s", returnedContent)
					}
				}
				if !bytes.Contains(response, []byte(nativeTransformations)) || !bytes.Contains(response, []byte("keep")) {
					t.Fatalf("response lost fields: %s", response)
				}
				latest := captured[len(captured)-1]
				for _, path := range []string{"system", "tools", "messages", "metadata", "thinking.block_binding"} {
					if gjson.GetBytes(latest, path).Raw != gjson.GetBytes(payload, path).Raw {
						t.Fatalf("changed %s: %s", path, latest)
					}
				}
				if turn > 0 {
					previous := gjson.GetBytes(captured[len(captured)-2], "messages").Array()
					current := gjson.GetBytes(latest, "messages").Array()
					for i, message := range previous {
						if message.Raw != current[i].Raw {
							t.Fatalf("earlier message %d changed", i)
						}
					}
				}
				if gjson.GetBytes(latest, "model").String() != model {
					t.Fatalf("model did not change: %s", latest)
				}
			}
			// a later user turn must not rebuild the earlier prefix
			next, _ := sjson.SetRawBytes([]byte(nativeHistoryFixture), "messages.-1", []byte(`{"role":"user","content":"second user"}`))
			_, _, err := e.prepareNativeHistory(ctx, a, exec.Request{Model: "claude-sonnet-5-5", Payload: next}, opts)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClaudeNativeHistoryPreflightRejectsBeforeNetwork(t *testing.T) {
	calls := 0
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })))
	for _, tc := range []struct {
		name    string
		cfg     *config.Config
		attrs   map[string]string
		payload string
		opts    exec.Options
	}{
		{name: "cross source", opts: exec.Options{SourceFormat: translator.FormatOpenAI}},
		{name: "cross response", opts: exec.Options{SourceFormat: translator.FormatClaude, ResponseFormat: translator.FormatOpenAI}},
		{name: "cloak", attrs: map[string]string{"cloak_mode": "always"}},
		{name: "obfuscate", attrs: map[string]string{"cloak_sensitive_words": "opaque"}},
		{name: "fingerprint", attrs: map[string]string{"fingerprint_profile": "claude-code-cli"}},
		{name: "mid system", attrs: map[string]string{"rebuild_mid_system_message": "true"}},
		{name: "identity header", attrs: map[string]string{"header:User-Agent": "fake cli"}},
		{name: "payload prefix", cfg: &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"messages.0.content": "changed"}}}}}},
		{name: "forced tool", payload: strings.Replace(nativeHistoryFixture, `"thinking":{`, `"tool_choice":{"type":"any"},"thinking":{`, 1)},
		{name: "invalid binding", payload: strings.Replace(nativeHistoryFixture, `"error"`, `"omit"`, 1)},
		{name: "bad betas", payload: strings.Replace(nativeHistoryFixture, `"betas":["thinking-binding-controls-2026-08-01","future-caller-beta"]`, `"betas":[7]`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := map[string]string{"api_key": "fake"}
			for k, v := range tc.attrs {
				attrs[k] = v
			}
			a := &auth.Auth{Provider: "claude", Attributes: attrs}
			opts := tc.opts
			if opts.SourceFormat == "" {
				opts.SourceFormat = translator.FormatClaude
			}
			opts.NativeHistory = exec.NativeHistoryPreserve
			payload := tc.payload
			if payload == "" {
				payload = nativeHistoryFixture
			}
			r := exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(payload)}
			e := NewClaudeExecutor(tc.cfg)
			if err := e.PreflightNativeHistory(ctx, r, opts, exec.NativeHistoryCredential{Attributes: attrs}); err == nil {
				t.Fatal("preflight accepted")
			}
			for _, call := range []func() error{func() error { _, err := e.Execute(ctx, a, r, opts); return err }, func() error { _, err := e.ExecuteStream(ctx, a, r, opts); return err }, func() error { _, err := e.CountTokens(ctx, a, r, opts); return err }} {
				if err := call(); err == nil {
					t.Fatal("execution accepted")
				}
			}
		})
	}
	if calls != 0 {
		t.Fatalf("network calls=%d", calls)
	}
}

func TestClaudeNativeHistoryBetweenToolsUsesCanonicalOwner(t *testing.T) {
	id := t.Name()
	registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-5-5", Type: "claude", Thinking: &registry.ThinkingSupport{BetweenToolsAllowed: true, Levels: []string{"low", "medium", "high"}}}})
	defer registry.GetGlobalRegistry().UnregisterClient(id)
	e := NewClaudeExecutor(&config.Config{})
	a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fake"}}
	opts := exec.Options{SourceFormat: translator.FormatClaude, NativeHistory: exec.NativeHistoryPreserve}
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"between_tools"},"output_config":{"effort":"high"}}`)
	body, _, err := e.prepareNativeHistory(context.Background(), a, exec.Request{Model: "claude-sonnet-5-5", Payload: payload}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "thinking.type").String() != "between_tools" {
		t.Fatal(string(body))
	}
	payload, _ = sjson.SetBytes(payload, "thinking.block_binding.prefix_mismatch_behavior", "error")
	if _, _, err := e.prepareNativeHistory(context.Background(), a, exec.Request{Model: "claude-sonnet-5-5", Payload: payload}, opts); err == nil {
		t.Fatal("between-tools binding accepted")
	}
}

func TestClaudeNativeHistoryProvider400IsNotRepaired(t *testing.T) {
	providerBody := `{"type":"error","error":{"type":"invalid_request_error","message":"invalid prefix"},"future":"keep"}`
	calls := 0
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(providerBody)), Request: r}, nil
	})))
	e := NewClaudeExecutor(&config.Config{})
	a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fake"}}
	req := exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(nativeHistoryFixture)}
	opts := exec.Options{SourceFormat: translator.FormatClaude, NativeHistory: exec.NativeHistoryPreserve}
	for _, call := range []func() error{func() error { _, err := e.Execute(ctx, a, req, opts); return err }, func() error { _, err := e.ExecuteStream(ctx, a, req, opts); return err }, func() error { _, err := e.CountTokens(ctx, a, req, opts); return err }} {
		var terminated *exec.RequestTerminatedError
		if err := call(); !errors.As(err, &terminated) || terminated.HTTPStatus != 400 || string(terminated.ResponseBody()) != providerBody {
			t.Fatalf("changed provider error: %v", err)
		}
	}
	if calls != 3 {
		t.Fatalf("repair retry calls=%d", calls)
	}
}

func TestClaudeNativeHistoryReportsActualAttemptTransformations(t *testing.T) {
	for _, method := range []string{"execute", "stream", "count", "truncated", "terminal"} {
		t.Run(method, func(t *testing.T) {
			pluginName := t.Name()
			plugin := &captureClaudeUsagePlugin{records: make(chan usage.Record, 32)}
			usage.RegisterNamedPlugin(pluginName, plugin)
			t.Cleanup(func() { usage.RegisterNamedPlugin(pluginName, noopClaudeUsagePlugin{}) })
			response := nativeTestResponse(method != "execute" && method != "count")
			if method == "count" {
				response = `{"input_tokens":7,"input_transformations":` + nativeTransformations + `}`
			}
			if method == "truncated" {
				response = strings.Split(response, "event: message_delta")[0]
			}
			if method == "terminal" {
				response = strings.Split(response, "event: message_delta")[0] + "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"fake failure\"}}\n\n"
			}
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			a := &auth.Auth{ID: t.Name(), Provider: "claude", Attributes: map[string]string{"api_key": "fake", "base_url": "https://fake-native.invalid"}}
			e := NewClaudeExecutor(&config.Config{})
			req := exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(nativeHistoryFixture)}
			opts := exec.Options{SourceFormat: translator.FormatClaude, NativeHistory: exec.NativeHistoryPreserve}
			switch method {
			case "execute":
				if _, err := e.Execute(ctx, a, req, opts); err != nil {
					t.Fatal(err)
				}
			case "count":
				if _, err := e.CountTokens(ctx, a, req, opts); err != nil {
					t.Fatal(err)
				}
			default:
				stream, err := e.ExecuteStream(ctx, a, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				sawFailure := false
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						sawFailure = true
					}
				}
				if sawFailure != (method == "truncated" || method == "terminal") {
					t.Fatalf("stream failure=%v", sawFailure)
				}
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for {
				select {
				case record := <-plugin.records:
					if record.AuthID != a.ID {
						continue
					}
					if record.InputTransformations.ModelBindingMismatch != 1 || record.InputTransformations.Unknown != 1 {
						t.Fatalf("actual usage count=%+v", record.InputTransformations)
					}
					if record.Failed != (method == "truncated" || method == "terminal") {
						t.Fatalf("usage failure=%v", record.Failed)
					}
					if method == "count" && usage.GenerateEnabled(record.Generate) {
						t.Fatal("count billed as generation")
					}
					return
				case <-deadline.C:
					t.Fatal("usage record missing")
				}
			}
		})
	}
}

type nativeCancelledBody struct {
	context context.Context
	prefix  *strings.Reader
	closed  chan struct{}
}

func (b *nativeCancelledBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	<-b.context.Done()
	return 0, b.context.Err()
}
func (b *nativeCancelledBody) Close() error { close(b.closed); return nil }

func TestClaudeNativeHistoryCancellationKeepsUsageAndContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan struct{})
	prefix := strings.Split(nativeTestResponse(true), "event: message_delta")[0]
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &nativeCancelledBody{context: r.Context(), prefix: strings.NewReader(prefix), closed: closed}, Request: r}, nil
	})
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
	a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fake", "base_url": "https://fake-native.invalid"}}
	e := NewClaudeExecutor(&config.Config{})
	stream, err := e.ExecuteStream(ctx, a, exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(nativeHistoryFixture)}, exec.Options{SourceFormat: translator.FormatClaude, NativeHistory: exec.NativeHistoryPreserve})
	if err != nil {
		t.Fatal(err)
	}
	first := <-stream.Chunks
	if !bytes.Contains(first.Payload, []byte("message_start")) {
		t.Fatal("first event missing")
	}
	cancel()
	sawCancelled := false
	for chunk := range stream.Chunks {
		if errors.Is(chunk.Err, context.Canceled) {
			sawCancelled = true
		}
	}
	if !sawCancelled {
		t.Fatal("cancel error missing")
	}
	<-closed
}

func TestClaudeNativeHistoryAcceptsLegacyAutoCloakDefault(t *testing.T) {
	a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat-fake-native", "cloak_mode": "auto"}}
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "sk-ant-oat-fake-native", Cloak: &config.CloakConfig{Mode: "auto"}}}}
	e := NewClaudeExecutor(cfg)
	body, _, err := e.prepareNativeHistory(context.Background(), a, exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(nativeHistoryFixture)}, exec.Options{SourceFormat: translator.FormatClaude, NativeHistory: exec.NativeHistoryPreserve})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"system", "tools", "messages", "metadata"} {
		if gjson.GetBytes(body, path).Raw != gjson.Get(nativeHistoryFixture, path).Raw {
			t.Fatalf("auto default changed %s", path)
		}
	}
}
