package test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	claudehandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const claudeAffinityBody = `{"model":"claude-sonnet-5-5","max_tokens":16000,"system":[{"type":"text","text":"caller system"}],"tools":[{"name":"mcp__native","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"thinking","thinking":"opaque","signature":"short opaque +/="},{"type":"redacted_thinking","data":"encrypted opaque"},{"type":"text","text":"first answer"}]},{"role":"user","content":"second"}],"thinking":{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"error"}}}`

type claudeAffinityTransport struct {
	mu        sync.Mutex
	accounts  []string
	bodies    [][]byte
	status    int
	terminal  bool
	truncated bool
}

func (u *claudeAffinityTransport) RoundTripperFor(*auth.Auth) http.RoundTripper { return u }
func (u *claudeAffinityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.accounts = append(u.accounts, r.Header.Get("Authorization"))
	u.bodies = append(u.bodies, body)
	status, terminal, truncated := u.status, u.terminal, u.truncated
	u.mu.Unlock()
	for name := range r.Header {
		if affinity.IsLocalHeader(name) {
			return nil, fmt.Errorf("local header escaped")
		}
	}
	if r.Header.Get("X-App") != "" || r.Header.Get("X-Stainless-Lang") != "" || r.Header.Get("X-Claude-Code-Session-Id") != "" {
		return nil, errors.New("false identity injected")
	}
	response := `{"id":"msg_fake","type":"message","model":"claude-sonnet-5-5","content":[{"type":"text","text":"answer"}],"future_field":"keep","input_transformations":[{"type":"thinking_dropped","path":"messages.1.content.0","reason":"future_reason"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	if status != 0 {
		response = `{"type":"error","error":{"type":"invalid_request_error","message":"fake invalid prefix"},"future_error":"keep"}`
		if status == 401 {
			response = `{"type":"error","error":{"type":"authentication_error","message":"fake unauthorized"},"future_error":"keep"}`
		}
	} else {
		status = 200
	}
	if gjson.GetBytes(body, "stream").Bool() && status == 200 {
		response = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_fake","model":"claude-sonnet-5-5","input_transformations":[{"type":"thinking_dropped","path":"messages.1.content.0","reason":"model_binding_mismatch"}],"usage":{"input_tokens":1}}}` + "\n\n"
		if terminal {
			response += "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"fake stream failure\"}}\n\n"
		} else if !truncated {
			response += "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"future\":\"keep\"},\"input_transformations\":[],\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		}
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") && status == 200 {
		response = `{"input_tokens":7,"future_count":"keep"}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
}

func claudeAffinityFixture(t *testing.T, cfg *config.Config, hosts ...handlers.PluginInterceptorHost) (*gin.Engine, *auth.Manager, *claudeAffinityTransport) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Routing.QuotaAwareChatAffinity = &config.QuotaAffinityConfig{Namespace: "native-test", StateDir: filepath.Join(t.TempDir(), "bindings")}
	manager := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(3, 0, 0)
	if err := manager.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.CloseQuotaAffinity() })
	upstream := &claudeAffinityTransport{}
	manager.SetRoundTripperProvider(upstream)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	for i, account := range []string{"a", "b"} {
		id := t.Name() + "-" + account
		a := &auth.Auth{ID: id, Provider: "claude", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "fake-" + account, "base_url": "https://native-fixture.invalid", "priority": fmt.Sprint(10 - i)}}
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-5-5", Type: "claude"}, {ID: "claude-opus-5-5", Type: "claude"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	if len(hosts) > 0 {
		base.SetPluginHost(hosts[0])
	}
	handler := claudehandlers.NewClaudeCodeAPIHandler(base)
	accessManager := access.NewManager()
	accessManager.SetProviders([]access.Provider{affinityAccessProvider{}})
	router := gin.New()
	router.Use(api.AuthMiddleware(accessManager))
	router.POST("/v1/messages", handler.ClaudeMessages)
	router.POST("/v1/messages/count_tokens", handler.ClaudeCountTokens)
	return router, manager, upstream
}

func claudeAffinityCall(router http.Handler, path, chat, intent, handle, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer fake-caller-a")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Beta", "thinking-binding-controls-2026-08-01")
	if chat != "" {
		request.Header.Set(affinity.ChatIDHeader, chat)
	}
	if intent != "" {
		request.Header.Set(affinity.ChatIntentHeader, intent)
	}
	if handle != "" {
		request.Header.Set(affinity.ChatHandleHeader, handle)
	}
	writer := httptest.NewRecorder()
	router.ServeHTTP(writer, request)
	return writer
}

func TestStrictClaudeAuthenticatedHTTPPreservesNativeWireAndPin(t *testing.T) {
	router, manager, upstream := claudeAffinityFixture(t, nil)
	first := claudeAffinityCall(router, "/v1/messages", "chat", "start", "", claudeAffinityBody)
	if first.Code != 200 || !strings.Contains(first.Body.String(), "future_field") {
		t.Fatalf("start=%d %s", first.Code, first.Body.String())
	}
	handle := first.Header().Get(affinity.ChatHandleHeader)
	if handle == "" {
		t.Fatal("no saved handle")
	}
	parsed, err := affinity.ParseHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	caller, _ := affinity.AuthenticatedCaller("fake-access", "Bearer fake-caller-a")
	chat, _ := affinity.NewChatID("chat")
	continuation, _ := manager.NewChatRequest(caller, chat, affinity.ContinueIntent, parsed)
	if err := manager.ValidateChatContinuation(context.Background(), continuation); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5"} {
		body := strings.Replace(claudeAffinityBody, "claude-sonnet-5-5", model, 1)
		got := claudeAffinityCall(router, "/v1/messages", "chat", "continue", handle, body)
		if got.Code != 200 || got.Header().Get(affinity.ChatHandleHeader) != handle {
			t.Fatalf("continue=%d %s", got.Code, got.Body.String())
		}
	}
	count := claudeAffinityCall(router, "/v1/messages/count_tokens", "chat", "continue", handle, claudeAffinityBody)
	if count.Code != 200 || !strings.Contains(count.Body.String(), "future_count") {
		t.Fatal(count.Body.String())
	}
	streamBody := strings.TrimSuffix(claudeAffinityBody, "}") + `,"stream":true}`
	stream := claudeAffinityCall(router, "/v1/messages", "chat", "continue", handle, streamBody)
	if stream.Code != 200 || !strings.Contains(stream.Body.String(), "input_transformations") || !strings.Contains(stream.Body.String(), "message_delta") {
		t.Fatal(stream.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	for i, account := range upstream.accounts {
		if account != "Bearer fake-a" {
			t.Fatalf("account %d=%s", i, account)
		}
		for _, path := range []string{"system", "tools", "messages", "thinking.block_binding"} {
			if gjson.GetBytes(upstream.bodies[i], path).Raw != gjson.Get(claudeAffinityBody, path).Raw {
				t.Fatalf("changed %s in request %d", path, i)
			}
		}
	}
}

func TestStrictClaudeFailuresNeverUseSecondCredential(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count", "terminal", "truncated"} {
		for _, status := range []int{400, 401, 429, 500, 503} {
			t.Run(fmt.Sprintf("%s-%d", mode, status), func(t *testing.T) {
				router, _, upstream := claudeAffinityFixture(t, nil)
				first := claudeAffinityCall(router, "/v1/messages", "chat", "start", "", claudeAffinityBody)
				handle := first.Header().Get(affinity.ChatHandleHeader)
				if handle == "" {
					t.Fatal(first.Body.String())
				}
				upstream.mu.Lock()
				upstream.status = status
				if mode == "terminal" {
					upstream.status = 0
					upstream.terminal = true
				}
				if mode == "truncated" {
					upstream.status = 0
					upstream.truncated = true
				}
				upstream.mu.Unlock()
				path, body := "/v1/messages", claudeAffinityBody
				if mode == "count" {
					path += "/count_tokens"
				}
				if mode == "stream" || mode == "terminal" || mode == "truncated" {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				got := claudeAffinityCall(router, path, "chat", "continue", handle, body)
				if mode != "terminal" && mode != "truncated" && got.Code != status {
					t.Fatalf("status=%d want=%d body=%s", got.Code, status, got.Body.String())
				}
				if status == 400 && mode != "terminal" && mode != "truncated" && !strings.Contains(got.Body.String(), "future_error") {
					t.Fatalf("provider body changed: %s", got.Body.String())
				}
				upstream.mu.Lock()
				defer upstream.mu.Unlock()
				if len(upstream.accounts) != 2 {
					t.Fatalf("unexpected retries=%v", upstream.accounts)
				}
				for _, account := range upstream.accounts {
					if account != "Bearer fake-a" {
						t.Fatalf("second credential: %v", upstream.accounts)
					}
				}
			})
		}
	}
}

func TestStrictClaudeConfigPreflightDoesNotCommitBinding(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"system": "changed"}}}}}
	router, manager, upstream := claudeAffinityFixture(t, cfg)
	bad := claudeAffinityCall(router, "/v1/messages", "chat", "start", "", claudeAffinityBody)
	if bad.Code == 200 || bad.Header().Get(affinity.ChatHandleHeader) != "" {
		t.Fatalf("config accepted: %d %s", bad.Code, bad.Body.String())
	}
	caller, _ := affinity.AuthenticatedCaller("fake-access", "Bearer fake-caller-a")
	chat, _ := affinity.NewChatID("chat")
	unknownHandle, _ := affinity.NewHandle()
	missing, _ := manager.NewChatRequest(caller, chat, affinity.ContinueIntent, unknownHandle)
	var local *affinity.Error
	if err := manager.ValidateChatRequest(context.Background(), missing); !errors.As(err, &local) || local.Reason() != affinity.BindingMissing {
		t.Fatalf("preflight committed a binding: %v", err)
	}
	cfg.Payload = config.PayloadConfig{}
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	good := claudeAffinityCall(router, "/v1/messages", "chat", "start", "", claudeAffinityBody)
	if good.Code != 200 {
		t.Fatal(good.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.accounts) != 1 {
		t.Fatalf("network before preflight %v", upstream.accounts)
	}
}

func TestNativeClaudeExplicitSDKAndHookCannotRewritePrefix(t *testing.T) {
	_, manager, upstream := claudeAffinityFixture(t, nil)
	caller, _ := affinity.AuthenticatedCaller("host", "test")
	chat, _ := affinity.NewChatID("hook")
	start, _ := manager.NewChatRequest(caller, chat, affinity.StartIntent, affinity.Handle{})
	opts := exec.Options{SourceFormat: translator.FormatClaude, Affinity: start, RequestAfterAuthInterceptor: func(_ context.Context, r exec.RequestAfterAuthInterceptRequest) exec.RequestAfterAuthInterceptResponse {
		return exec.RequestAfterAuthInterceptResponse{Body: []byte(strings.Replace(string(r.Body), "caller system", "changed", 1))}
	}}
	_, err := manager.Execute(context.Background(), []string{"claude"}, exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(claudeAffinityBody)}, opts)
	if err == nil {
		t.Fatal("hook changed prefix")
	}
	// the explicit SDK policy is also active with no binding profile request
	opts = exec.Options{SourceFormat: translator.FormatOpenAI, NativeHistory: exec.NativeHistoryPreserve}
	_, err = manager.Execute(context.Background(), []string{"claude"}, exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(claudeAffinityBody)}, opts)
	if err == nil {
		t.Fatal("cross-format explicit policy accepted")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.accounts) != 0 {
		t.Fatalf("preflight used network %v", upstream.accounts)
	}
}

type claudeWithoutPreservation struct{ auth.ProviderExecutor }

func (claudeWithoutPreservation) LocalBuiltinProvider() string { return "claude" }

func TestStrictClaudeRequiresVerifiedExecutorCapability(t *testing.T) {
	router, manager, upstream := claudeAffinityFixture(t, nil)
	manager.RegisterExecutor(claudeWithoutPreservation{runtimeexecutor.NewClaudeExecutor(&config.Config{})})
	got := claudeAffinityCall(router, "/v1/messages", "chat", "start", "", claudeAffinityBody)
	if got.Code != 400 || !strings.Contains(got.Body.String(), "chat_prefix_transform_unsupported") || got.Header().Get(affinity.ChatHandleHeader) != "" {
		t.Fatalf("unverified executor accepted: %d %s", got.Code, got.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.accounts) != 0 {
		t.Fatal("unverified executor used network")
	}
}

func TestClaudeLegacyAndExplicitNativeErrorRouting(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprint(preserve), func(t *testing.T) {
			_, manager, upstream := claudeAffinityFixture(t, nil)
			upstream.status = 401
			opts := exec.Options{SourceFormat: translator.FormatClaude}
			if preserve {
				opts.NativeHistory = exec.NativeHistoryPreserve
			}
			_, err := manager.Execute(context.Background(), []string{"claude"}, exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(claudeAffinityBody)}, opts)
			if err == nil {
				t.Fatal("expected fake failure")
			}
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			want := 2
			if preserve {
				want = 1
			}
			if len(upstream.accounts) != want {
				t.Fatalf("accounts=%v want=%d", upstream.accounts, want)
			}
		})
	}
}

func TestStrictClaudeOfficialAPIKeyUsesExactTransportAuthorization(t *testing.T) {
	_, manager, upstream := claudeAffinityFixture(t, nil)
	// the production origin stays intact; the existing injected transport prevents provider calls
	a, _ := manager.GetByID(t.Name() + "-a")
	a.Attributes["base_url"] = "https://api.anthropic.com"
	_, _ = manager.Update(context.Background(), a)
	manager.SetRoundTripperProvider(claudeOfficialTransport{t, upstream})
	caller, _ := affinity.AuthenticatedCaller("host", "test")
	chat, _ := affinity.NewChatID("official")
	start, _ := manager.NewChatRequest(caller, chat, affinity.StartIntent, affinity.Handle{})
	_, err := manager.Execute(context.Background(), []string{"claude"}, exec.Request{Model: "claude-sonnet-5-5", Payload: []byte(claudeAffinityBody)}, exec.Options{SourceFormat: translator.FormatClaude, Affinity: start})
	if err != nil {
		t.Fatal(err)
	}
}

type claudeOfficialTransport struct {
	t        *testing.T
	upstream *claudeAffinityTransport
}

func (u claudeOfficialTransport) RoundTripperFor(*auth.Auth) http.RoundTripper { return u }
func (u claudeOfficialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.anthropic.com" || r.Header.Get("X-Api-Key") != "fake-a" || r.Header.Get("Authorization") != "" {
		u.t.Fatalf("incorrect actual key authorization: %v %v", r.URL, r.Header)
	}
	return u.upstream.RoundTrip(r)
}

func TestStrictClaudeOAuthDefaultDoesNotAcquireCLIIdentity(t *testing.T) {
	router, manager, upstream := claudeAffinityFixture(t, nil)
	a, _ := manager.GetByID(t.Name() + "-a")
	delete(a.Attributes, "api_key")
	a.Attributes["auth_kind"] = auth.AuthKindOAuth
	proof, err := affinity.AcquiredOAuthProvenance("claude", "fake-principal", "sk-ant-oat-fake-a")
	if err != nil {
		t.Fatal(err)
	}
	a.Metadata = map[string]any{"access_token": "sk-ant-oat-fake-a", "account_uuid": "fake-principal", "oauth_provenance": proof}
	if _, err := manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	first := claudeAffinityCall(router, "/v1/messages", "oauth-chat", "start", "", claudeAffinityBody)
	if first.Code != 200 {
		t.Fatalf("OAuth start=%d %s", first.Code, first.Body.String())
	}
	handle := first.Header().Get(affinity.ChatHandleHeader)
	if handle == "" {
		t.Fatal("OAuth pin missing")
	}
	count := claudeAffinityCall(router, "/v1/messages/count_tokens", "oauth-chat", "continue", handle, claudeAffinityBody)
	if count.Code != 200 {
		t.Fatal(count.Body.String())
	}
	current, _ := manager.GetByID(a.ID)
	if current.Metadata["device_id"] != nil || current.Metadata["device_id_pool"] != nil || current.Metadata["claude_account_profile_checked_at"] != nil {
		t.Fatal("strict request acquired CLI identity state")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.accounts) != 2 {
		t.Fatal(upstream.accounts)
	}
	for _, account := range upstream.accounts {
		if account != "Bearer sk-ant-oat-fake-a" {
			t.Fatalf("OAuth transport account changed: %v", upstream.accounts)
		}
	}
}
