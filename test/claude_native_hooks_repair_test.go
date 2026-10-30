package test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type destructiveNativeHost struct {
	armed bool
	mode  string
	calls int
}

func (h *destructiveNativeHost) HasRequestInterceptors() bool { return h.armed }
func (*destructiveNativeHost) HasStreamInterceptors() bool    { return false }
func (h *destructiveNativeHost) InterceptRequestBeforeAuth(_ context.Context, r pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	h.calls++
	switch h.mode {
	case "before-rewrite":
		return pluginapi.RequestInterceptResponse{Body: []byte(`{"model":"claude-sonnet-5-5","max_tokens":1,"messages":[]}`)}
	case "in-place":
		for i := range r.Body {
			r.Body[i] = ' '
		}
	}
	return pluginapi.RequestInterceptResponse{}
}
func (h *destructiveNativeHost) InterceptRequestAfterAuth(_ context.Context, r pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	h.calls++
	r.Headers.Del("Anthropic-Beta")
	return pluginapi.RequestInterceptResponse{ClearHeaders: []string{"Anthropic-Beta"}}
}
func (*destructiveNativeHost) InterceptResponse(_ context.Context, r pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{Headers: r.ResponseHeaders, Body: r.Body}
}
func (*destructiveNativeHost) InterceptStreamChunk(_ context.Context, r pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	return pluginapi.StreamChunkInterceptResponse{Headers: r.ResponseHeaders, Body: r.Body}
}

func TestNativeHTTPHostHooksRejectedBeforeMutation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, mode := range []string{"before-rewrite", "in-place", "after-beta-removal"} {
			for _, operation := range []string{"execute", "stream", "count"} {
				t.Run(fmt.Sprintf("plugins-%t/%s/%s", enabled, mode, operation), func(t *testing.T) {
					cfg := &config.Config{}
					host := &destructiveNativeHost{mode: mode}
					router, _, upstream := claudeAffinityFixture(t, cfg, host)
					saved := claudeAffinityCall(router, "/v1/messages", "saved", "start", "", claudeAffinityBody)
					if saved.Code != 200 {
						t.Fatal(saved.Body.String())
					}
					cfg.Plugins.Enabled = enabled
					host.armed = true
					path := "/v1/messages"
					body := claudeAffinityBody
					chat := "new"
					intent := "start"
					handle := ""
					if operation == "stream" {
						body = strings.TrimSuffix(body, "}") + `,"stream":true}`
					}
					if operation == "count" {
						path += "/count_tokens"
						chat = "saved"
						intent = "continue"
						handle = saved.Header().Get(affinity.ChatHandleHeader)
					}
					got := claudeAffinityCall(router, path, chat, intent, handle, body)
					if got.Code != 400 || !strings.Contains(got.Body.String(), "chat_prefix_transform_unsupported") {
						t.Fatal("native hook not rejected", got.Code, got.Body.String())
					}
					if host.calls != 0 {
						t.Fatal("hook ran before rejection", host.calls)
					}
					upstream.mu.Lock()
					defer upstream.mu.Unlock()
					if len(upstream.accounts) != 1 {
						t.Fatal("rejected hook dispatched", upstream.accounts)
					}
				})
			}
		}
	}
}

func TestNativeDirectSDKHooksRejectedForEveryDispatch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, operation := range []string{"execute", "stream", "count"} {
			for _, mode := range []string{"body-rewrite", "in-place", "after-beta-removal"} {
				t.Run(fmt.Sprintf("plugins-%t/%s/%s", enabled, operation, mode), func(t *testing.T) {
					cfg := &config.Config{}
					_, m, upstream := claudeAffinityFixture(t, cfg)
					cfg.Plugins.Enabled = enabled
					called := false
					payload := []byte(claudeAffinityBody)
					opts := exec.Options{SourceFormat: translator.FormatClaude, NativeHistory: exec.NativeHistoryPreserve, Headers: http.Header{"Anthropic-Beta": {"thinking-binding-controls-2026-08-01"}}, RequestAfterAuthInterceptor: func(_ context.Context, r exec.RequestAfterAuthInterceptRequest) exec.RequestAfterAuthInterceptResponse {
						called = true
						if mode == "in-place" {
							for i := range r.Body {
								r.Body[i] = ' '
							}
						}
						if mode == "after-beta-removal" {
							return exec.RequestAfterAuthInterceptResponse{ClearHeaders: []string{"Anthropic-Beta"}}
						}
						return exec.RequestAfterAuthInterceptResponse{Body: []byte(`{"messages":[]}`)}
					}}
					req := exec.Request{Model: "claude-sonnet-5-5", Payload: payload}
					var err error
					switch operation {
					case "execute":
						_, err = m.Execute(context.Background(), []string{"claude"}, req, opts)
					case "stream":
						_, err = m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
					case "count":
						_, err = m.ExecuteCount(context.Background(), []string{"claude"}, req, opts)
					}
					if err == nil || called || string(payload) != claudeAffinityBody || opts.Headers.Get("Anthropic-Beta") == "" {
						t.Fatal("native SDK hook ran or changed controls", err)
					}
					upstream.mu.Lock()
					defer upstream.mu.Unlock()
					if len(upstream.accounts) != 0 {
						t.Fatal("SDK hook dispatched")
					}
				})
			}
		}
	}
}
