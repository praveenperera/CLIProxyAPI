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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openai "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	quota "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type affinityAccessProvider struct{}

func (affinityAccessProvider) Identifier() string { return "fake-access" }
func (affinityAccessProvider) Authenticate(_ context.Context, r *http.Request) (*access.Result, *access.AuthError) {
	principal := r.Header.Get("Authorization")
	if principal != "Bearer fake-caller-a" && principal != "Bearer fake-caller-b" {
		return nil, access.NewInvalidCredentialError()
	}
	return &access.Result{Provider: "fake-access", Principal: principal}, nil
}

type affinityUpstream struct {
	mu       sync.Mutex
	accounts []string
	status   int
	terminal bool
}

func (u *affinityUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.accounts = append(u.accounts, r.Header.Get("Authorization"))
	status, terminal := u.status, u.terminal
	u.mu.Unlock()
	for name := range r.Header {
		if affinity.IsLocalHeader(name) {
			http.Error(w, "local header leaked", 500)
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"fake upstream failure"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Codex-Primary-Used-Percent", "20")
	w.Header().Set("X-Codex-Primary-Window-Minutes", "60")
	w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "3600")
	_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"fake-response\",\"output\":[]}}\n\n")
	if terminal {
		_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"fake exhausted\",\"resets_in_seconds\":3600}}}\n\n")
		return
	}
	_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fake-response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
}
func (u *affinityUpstream) snapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.accounts...)
}

func affinityHTTPFixture(t *testing.T) (*gin.Engine, *auth.Manager, *affinityUpstream, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	upstream := &affinityUpstream{}
	server := httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(server.Close)
	cfg := &config.Config{Routing: config.RoutingConfig{QuotaAwareChatAffinity: &config.QuotaAffinityConfig{Namespace: "fake-test", StateDir: filepath.Join(t.TempDir(), "bindings")}}}
	manager := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(3, 0, 0)
	if err := manager.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.CloseQuotaAffinity() })
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	model := "gpt-5.4"
	for i, account := range []string{"a", "b"} {
		id := t.Name() + "-" + account
		_, err := manager.Register(context.Background(), &auth.Auth{ID: id, Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "fake-upstream-" + account, "base_url": server.URL, "priority": fmt.Sprint(10 - i)}})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}, {ID: "gpt-5.4-mini"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	responses := openai.NewOpenAIResponsesAPIHandler(base)
	accessManager := access.NewManager()
	accessManager.SetProviders([]access.Provider{affinityAccessProvider{}})
	router := gin.New()
	router.Use(api.AuthMiddleware(accessManager))
	router.POST("/v1/responses", responses.Responses)
	router.POST("/v1/responses/compact", responses.Compact)
	router.GET("/v1/responses", responses.ResponsesWebsocket)
	return router, manager, upstream, model
}

func affinityHTTPCall(router http.Handler, caller, chat, intent, handle, model string, stream bool) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"model":%q,"input":"hello","stream":%v}`, model, stream)
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", caller)
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

func TestQuotaAffinityHTTPRealExecutorNeverUsesSecondAccount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{401, 402, 403, 429, 500, 503} {
			t.Run(fmt.Sprintf("stream-%v-status-%d", stream, status), func(t *testing.T) {
				router, _, upstream, model := affinityHTTPFixture(t)
				first := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "start", "", model, false)
				handle := first.Header().Get(affinity.ChatHandleHeader)
				if first.Code != 200 || handle == "" {
					t.Fatalf("start %d %s", first.Code, first.Body.String())
				}
				upstream.mu.Lock()
				upstream.status = status
				upstream.mu.Unlock()
				next := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "continue", handle, "gpt-5.4-mini", stream)
				if next.Code == 200 && !strings.Contains(next.Body.String(), "error") {
					t.Fatalf("failure became success %s", next.Body.String())
				}
				for _, account := range upstream.snapshot() {
					if account != "Bearer fake-upstream-a" {
						t.Fatalf("second account dispatched %v", upstream.snapshot())
					}
				}
				if len(upstream.snapshot()) != 2 {
					t.Fatalf("unexpected network retries %v", upstream.snapshot())
				}
			})
		}
	}
}

func TestQuotaAffinityHTTPIdentityHandleIsolationAndStreamTerminal(t *testing.T) {
	router, manager, upstream, model := affinityHTTPFixture(t)
	missing := affinityHTTPCall(router, "Bearer fake-caller-a", "", "", "", model, false)
	if missing.Code != 400 || len(upstream.snapshot()) != 0 {
		t.Fatal("missing identity dispatched")
	}
	first := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "start", "", model, true)
	handle := first.Header().Get(affinity.ChatHandleHeader)
	if first.Code != 200 || handle == "" || !strings.Contains(first.Body.String(), "response.completed") {
		t.Fatalf("stream start %d %s", first.Code, first.Body.String())
	}
	replay := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "start", "", model, false)
	if replay.Code != 200 || replay.Header().Get(affinity.ChatHandleHeader) != handle {
		t.Fatal("start did not retain handle")
	}
	isolated := affinityHTTPCall(router, "Bearer fake-caller-b", "chat", "continue", handle, model, false)
	if isolated.Code != 410 {
		t.Fatalf("caller isolation status %d %s", isolated.Code, isolated.Body.String())
	}
	if _, err := affinity.ParseHandle(handle); err != nil {
		t.Fatal(err)
	}
	view, err := manager.QuotaView(t.Name()+"-a", model)
	if err != nil {
		t.Fatal(err)
	}
	if view.Knowledge() != quota.Known || view.Freshness() != quota.Fresh {
		t.Fatal("real passive header receipt was not ingested")
	}
	if _, ok := view.NextReset(); !ok {
		t.Fatal("passive reset missing")
	}
	upstream.mu.Lock()
	upstream.terminal = true
	upstream.mu.Unlock()
	failed := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "continue", handle, model, true)
	if !strings.Contains(failed.Body.String(), "error") {
		t.Fatalf("terminal error missing %d %s", failed.Code, failed.Body.String())
	}
	retry := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "continue", handle, model, false)
	if retry.Code != 503 {
		t.Fatalf("exhausted continuation %d %s", retry.Code, retry.Body.String())
	}
	if len(upstream.snapshot()) != 3 {
		t.Fatalf("terminal rerouted %v", upstream.snapshot())
	}
}

func TestQuotaAffinityWebsocketReconnectRetainsSavedBinding(t *testing.T) {
	router, _, upstream, model := affinityHTTPFixture(t)
	first := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "start", "", model, false)
	handle := first.Header().Get(affinity.ChatHandleHeader)
	if handle == "" {
		t.Fatal(first.Body.String())
	}
	downstream := httptest.NewServer(router)
	defer downstream.Close()
	url := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/v1/responses"
	headers := http.Header{"Authorization": {"Bearer fake-caller-a"}}
	headers.Set(affinity.ChatIDHeader, "chat")
	headers.Set(affinity.ChatHandleHeader, handle)
	for turn := 0; turn < 2; turn++ {
		conn, response, err := websocket.DefaultDialer.Dial(url, headers)
		if err != nil {
			if response != nil {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("upgrade %v %s", err, body)
			}
			t.Fatal(err)
		}
		if response.Header.Get(affinity.ChatHandleHeader) != handle {
			t.Fatal("upgrade lost saved handle")
		}
		if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)); err != nil {
			t.Fatal(err)
		}
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Fatal(errRead)
			}
			if strings.Contains(string(payload), "response.completed") {
				break
			}
			if strings.Contains(string(payload), `"type":"error"`) {
				t.Fatalf("websocket error %s", payload)
			}
		}
		_ = conn.Close()
	}
	for _, account := range upstream.snapshot() {
		if account != "Bearer fake-upstream-a" {
			t.Fatalf("reconnect rerouted %v", upstream.snapshot())
		}
	}
	// a websocket start cannot commit a credential before the model arrives
	headers.Del(affinity.ChatHandleHeader)
	headers.Set(affinity.ChatIntentHeader, "start")
	conn, response, err := websocket.DefaultDialer.Dial(url, headers)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || response == nil || response.StatusCode != 400 {
		t.Fatal("websocket cold start accepted")
	}
}

func TestQuotaAffinityRealWebsocketTransportAndHTTPLegacy(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprint(strict), func(t *testing.T) {
			attempts := make(chan string, 8)
			upgrader := websocket.Upgrader{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				account := r.Header.Get("Authorization")
				attempts <- account
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				if account == "Bearer fake-a" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"fake quota","resets_in_seconds":3600}}`))
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"fake","status":"completed","output":[]}}`))
			}))
			defer upstream.Close()
			cfg := &config.Config{Routing: config.RoutingConfig{QuotaAwareChatAffinity: &config.QuotaAffinityConfig{Namespace: "test", StateDir: filepath.Join(t.TempDir(), "bindings")}}}
			m := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
			m.SetConfig(cfg)
			m.RegisterExecutor(runtimeexecutor.NewCodexWebsocketsExecutor(cfg))
			if err := m.ConfigureQuotaAffinity(cfg); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = m.CloseQuotaAffinity() }()
			for i, id := range []string{t.Name() + "-a", t.Name() + "-b"} {
				key := "fake-a"
				if i == 1 {
					key = "fake-b"
				}
				_, err := m.Register(context.Background(), &auth.Auth{ID: id, Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": key, "base_url": upstream.URL, "priority": fmt.Sprint(10 - i)}})
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			opts := exec.Options{Stream: true, SourceFormat: sdktranslator.FromString("openai-response")}
			if strict {
				caller, _ := affinity.NewCallerScope("test")
				chat, _ := affinity.NewChatID("chat")
				opts.Affinity, _ = m.NewChatRequest(caller, chat, affinity.StartIntent, affinity.Handle{})
			}
			stream, err := m.ExecuteStream(context.Background(), []string{"codex"}, exec.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`)}, opts)
			if stream != nil {
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						err = chunk.Err
					}
				}
			}
			if strict && err == nil {
				t.Fatal("strict quota error became success")
			}
			if !strict && err != nil {
				t.Fatal(err)
			}
			var accounts []string
			for {
				select {
				case account := <-attempts:
					accounts = append(accounts, account)
				default:
					goto drained
				}
			}
		drained:
			if strict {
				if len(accounts) != 1 || accounts[0] != "Bearer fake-a" {
					t.Fatalf("strict failover %v", accounts)
				}
			} else if len(accounts) != 2 || accounts[1] != "Bearer fake-b" {
				t.Fatalf("legacy changed %v", accounts)
			}
		})
	}
}

func TestStrictClaudePolicyRejectsUnsupportedNativeFormats(t *testing.T) {
	cfg := &config.Config{}
	e := runtimeexecutor.NewClaudeExecutor(cfg)
	opts := exec.Options{NativeHistory: exec.NativeHistoryPreserve}
	a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fake"}}
	req := exec.Request{Model: "claude-test"}
	for _, call := range []func() error{func() error { _, err := e.Execute(context.Background(), a, req, opts); return err }, func() error { _, err := e.ExecuteStream(context.Background(), a, req, opts); return err }, func() error { _, err := e.CountTokens(context.Background(), a, req, opts); return err }} {
		var local *affinity.Error
		if err := call(); !errors.As(err, &local) || local.Reason() != affinity.PrefixTransformUnsupported {
			t.Fatalf("strict flag ignored %v", err)
		}
	}
}

func TestQuotaAffinityFinalAuthHeadersCannotChangeAccount(t *testing.T) {
	router, manager, upstream, model := affinityHTTPFixture(t)
	first := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "start", "", model, false)
	handle := first.Header().Get(affinity.ChatHandleHeader)
	if handle == "" {
		t.Fatal(first.Body.String())
	}
	a, _ := manager.GetByID(t.Name() + "-a")
	a.Attributes["header:Authorization"] = "Bearer fake-upstream-b"
	if _, err := manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	next := affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "continue", handle, model, false)
	if next.Code != 503 || !strings.Contains(next.Body.String(), "chat_credential_changed") {
		t.Fatalf("credential header override %d %s", next.Code, next.Body.String())
	}
	if len(upstream.snapshot()) != 1 {
		t.Fatalf("header dispatched another credential %v", upstream.snapshot())
	}
	manager.Remove(context.Background(), a.ID)
	registry.GetGlobalRegistry().UnregisterClient(a.ID)
	registry.GetGlobalRegistry().UnregisterClient(t.Name() + "-b")
	next = affinityHTTPCall(router, "Bearer fake-caller-a", "chat", "continue", handle, model, false)
	if next.Code != 503 || !strings.Contains(next.Body.String(), "chat_credential_missing") {
		t.Fatalf("eligible list hid binding %d %s", next.Code, next.Body.String())
	}
}

type quotaFixtureTransport struct {
	mu    sync.Mutex
	calls []string
}

func (p *quotaFixtureTransport) RoundTripperFor(*auth.Auth) http.RoundTripper { return p }
func (p *quotaFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.calls = append(p.calls, r.URL.String())
	p.mu.Unlock()
	for name := range r.Header {
		if affinity.IsLocalHeader(name) {
			return nil, errors.New("local header entered usage transport")
		}
	}
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.Body != nil {
		return nil, errors.New("usage was not read-only")
	}
	body := ""
	switch r.URL.String() {
	case "https://chatgpt.com/backend-api/wham/usage":
		if r.Header.Get("Authorization") != "Bearer fake-codex-token" || r.Header.Get("Chatgpt-Account-Id") != "fake-codex-principal" {
			return nil, errors.New("Codex usage credential was not injected")
		}
		body = `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":3600,"reset_after_seconds":3600}}}`
	case "https://api.anthropic.com/api/oauth/usage":
		if r.Header.Get("Authorization") != "Bearer fake-claude-token" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
			return nil, errors.New("Claude usage credential was not injected")
		}
		body = fmt.Sprintf(`{"five_hour":{"utilization":20,"resets_at":%q}}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	default:
		return nil, errors.New("unexpected usage destination")
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestQuotaAffinityUsageUsesRealBuiltinAuthenticatedTransport(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{QuotaAwareChatAffinity: &config.QuotaAffinityConfig{Namespace: "fake-usage", StateDir: filepath.Join(t.TempDir(), "state")}}}
	manager := auth.NewManager(nil, nil, nil)
	if err := manager.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.CloseQuotaAffinity() }()
	transport := &quotaFixtureTransport{}
	manager.SetRoundTripperProvider(transport)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	for _, provider := range []string{"codex", "claude"} {
		token, principal := "fake-"+provider+"-token", "fake-"+provider+"-principal"
		proof, err := affinity.AcquiredOAuthProvenance(provider, principal, token)
		if err != nil {
			t.Fatal(err)
		}
		identity := "account_id"
		if provider == "claude" {
			identity = "account_uuid"
		}
		a := &auth.Auth{ID: "fake-" + provider, Provider: provider, Status: auth.StatusActive, Attributes: map[string]string{auth.AttributeAuthKind: auth.AuthKindOAuth}, Metadata: map[string]any{"access_token": token, identity: principal, "oauth_provenance": proof}}
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	manager.PollQuotaOnce(context.Background())
	for _, provider := range []string{"codex", "claude"} {
		view, err := manager.QuotaView("fake-"+provider, "model")
		if err != nil || view.Freshness() != quota.Fresh || view.Knowledge() != quota.Known {
			t.Fatalf("%s usage observation was not ingested: %v", provider, err)
		}
		a, _ := manager.GetByID("fake-" + provider)
		a.Disabled = true
		if _, err := manager.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	manager.PollQuotaOnce(context.Background())
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.calls) != 2 {
		t.Fatalf("usage calls = %d, want 2", len(transport.calls))
	}
}
