package test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func repairManager(t *testing.T, origin string) (*auth.Manager, *config.Config, []string) {
	t.Helper()
	cfg := &config.Config{Routing: config.RoutingConfig{QuotaAwareChatAffinity: &config.QuotaAffinityConfig{Namespace: "repair", StateDir: filepath.Join(t.TempDir(), "bindings")}}}
	m := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
	m.SetConfig(cfg)
	if err := m.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseQuotaAffinity() })
	ids := []string{t.Name() + "-a", t.Name() + "-b"}
	for i, id := range ids {
		_, err := m.Register(context.Background(), &auth.Auth{ID: id, Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": fmt.Sprintf("fake-%d", i), "base_url": origin, "priority": fmt.Sprint(10 - i)}})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m, cfg, ids
}
func repairChat(t *testing.T, m *auth.Manager, callerName, chatName string) affinity.Request {
	t.Helper()
	caller, _ := affinity.AuthenticatedCaller("sdk-host", callerName)
	chat, _ := affinity.NewChatID(chatName)
	r, err := m.NewChatRequest(caller, chat, affinity.StartIntent, affinity.Handle{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStrictWebsocketReplacementAndSDKCallerCollision(t *testing.T) {
	for _, scenario := range []string{"replacement", "legacy-replacement", "caller-collision", "verified-refresh"} {
		t.Run(scenario, func(t *testing.T) {
			type frame struct {
				credential string
				connection int
			}
			frames := make(chan frame, 8)
			releaseFirst := make(chan struct{})
			var mu sync.Mutex
			connection := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				mu.Lock()
				connection++
				number := connection
				mu.Unlock()
				credential := r.Header.Get("Authorization")
				for {
					_, _, err := conn.ReadMessage()
					if err != nil {
						return
					}
					frames <- frame{credential, number}
					if number == 1 {
						<-releaseFirst
					}
					if strings.HasSuffix(scenario, "replacement") && number == 1 {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":60,"reset_after_seconds":3600}}}`))
					}
					if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"fake","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			releaseFirstOnce := sync.OnceFunc(func() { close(releaseFirst) })
			defer releaseFirstOnce()
			m, cfg, ids := repairManager(t, server.URL)
			if scenario == "verified-refresh" {
				a, _ := m.GetByID(ids[0])
				a.Attributes = map[string]string{"auth_kind": auth.AuthKindOAuth, "base_url": server.URL, "priority": "10"}
				proof, err := affinity.AcquiredOAuthProvenance("codex", "account-a", "fake-0")
				if err != nil {
					t.Fatal(err)
				}
				a.Metadata = map[string]any{"account_id": "account-a", "access_token": "fake-0", "oauth_provenance": proof}
				if _, err := m.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			ws := runtimeexecutor.NewCodexWebsocketsExecutor(cfg)
			m.RegisterExecutor(ws)
			session := t.Name() + "-shared-session"
			defer ws.CloseExecutionSession(session)
			req := exec.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`)}
			opts := exec.Options{SourceFormat: translator.FormatOpenAIResponse, Affinity: repairChat(t, m, "caller-a", "first"), Metadata: map[string]any{exec.ExecutionSessionMetadataKey: session}}
			if scenario == "legacy-replacement" {
				opts.Affinity = affinity.Request{}
			}
			firstDone := make(chan error, 1)
			go func() { _, err := m.Execute(context.Background(), []string{"codex"}, req, opts); firstDone <- err }()
			var first frame
			select {
			case first = <-frames:
			case err := <-firstDone:
				t.Fatal("first execution stopped before websocket frame", err)
			}
			if first.credential != "Bearer fake-0" {
				t.Fatal("wrong first credential", first)
			}
			second := opts
			if strings.HasSuffix(scenario, "replacement") {
				a, _ := m.GetByID(ids[0])
				a.Attributes["api_key"] = "fake-replaced"
				if _, err := m.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
				second.Affinity = repairChat(t, m, "caller-a", "new-chat")
			} else if scenario == "verified-refresh" {
				releaseFirstOnce()
				if err := <-firstDone; err != nil {
					t.Fatal(err)
				}
				a, _ := m.GetByID(ids[0])
				a.Metadata["access_token"] = "fake-refreshed"
				proof, err := affinity.AcquiredOAuthProvenance("codex", "account-a", "fake-refreshed")
				if err != nil {
					t.Fatal(err)
				}
				a.Metadata["oauth_provenance"] = proof
				if _, err := m.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			} else {
				second.Affinity = repairChat(t, m, "caller-b", "first")
			}
			secondDone := make(chan error, 1)
			go func() { _, err := m.Execute(context.Background(), []string{"codex"}, req, second); secondDone <- err }()
			if scenario != "verified-refresh" {
				releaseFirstOnce()
				if err := <-firstDone; err != nil {
					t.Fatal(err)
				}
			}
			var next frame
			select {
			case next = <-frames:
			case err := <-secondDone:
				t.Fatal("second execution stopped before websocket frame", err)
			}
			if next.connection == first.connection {
				t.Fatal("different lineage or caller reused old socket")
			}
			want := "Bearer fake-0"
			if strings.HasSuffix(scenario, "replacement") {
				want = "Bearer fake-replaced"
			}
			if scenario == "verified-refresh" {
				want = "Bearer fake-refreshed"
			}
			if next.credential != want {
				t.Fatal("wire credential does not match binding", next)
			}
			if err := <-secondDone; err != nil {
				t.Fatal(err)
			}
			// a required-socket continuation from the old caller/chat cannot send on the new binding
			required := exec.WithRequiredUpstreamWebsocket(context.Background())
			_, err := m.Execute(required, []string{"codex"}, req, opts)
			if scenario == "verified-refresh" {
				if err != nil {
					t.Fatal("verified refresh lost chat continuity", err)
				}
				frame := <-frames
				if frame.connection != next.connection || frame.credential != want {
					t.Fatal("required socket did not keep actual refreshed handshake")
				}
			} else {
				if err == nil {
					t.Fatal("required socket accepted another transport owner")
				}
				select {
				case unexpected := <-frames:
					t.Fatal("required socket sent without owner proof", unexpected)
				default:
				}
			}
		})
	}
}

type quotaEventBody struct {
	first   []byte
	rest    io.Reader
	entered chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *quotaEventBody) Read(dst []byte) (int, error) {
	if len(b.first) > 0 {
		n := copy(dst, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	b.once.Do(func() { b.entered <- struct{}{}; <-b.release })
	return b.rest.Read(dst)
}
func (*quotaEventBody) Close() error { return nil }

type eventQuotaTransport struct {
	entered chan struct{}
	release chan struct{}
	used    int
}

func (t *eventQuotaTransport) RoundTripperFor(*auth.Auth) http.RoundTripper { return t }
func (t *eventQuotaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	event := fmt.Sprintf("data: {\"type\":\"codex.rate_limits\",\"rate_limits\":{\"primary\":{\"used_percent\":%d,\"window_minutes\":60,\"reset_after_seconds\":3600}}}\n\n", t.used)
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fake\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &quotaEventBody{first: []byte(event), rest: strings.NewReader(terminal), entered: t.entered, release: t.release}, Request: r}, nil
}
func TestCodexHTTPEventOnlyQuotaAffectsAdmissionBeforeBodyEnd(t *testing.T) {
	for _, used := range []int{20, 100} {
		t.Run(fmt.Sprint(used), func(t *testing.T) {
			m, cfg, ids := repairManager(t, "https://fake.invalid")
			m.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
			transport := &eventQuotaTransport{entered: make(chan struct{}, 2), release: make(chan struct{}), used: used}
			m.SetRoundTripperProvider(transport)
			done := make(chan error, 1)
			req := exec.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`)}
			go func() {
				_, err := m.Execute(context.Background(), []string{"codex"}, req, exec.Options{SourceFormat: translator.FormatOpenAIResponse, Metadata: map[string]any{exec.PinnedAuthMetadataKey: ids[0]}})
				done <- err
			}()
			<-transport.entered
			view, err := m.QuotaView(ids[0], "gpt-5.4")
			if err != nil || len(view.Windows()) == 0 || view.Denied() != (used == 100) {
				t.Fatal("event not observed before ReadAll completes", view, err)
			}
			close(transport.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			// rank the next explicit chat with no quota headers on either response
			var selected string
			opts := exec.Options{SourceFormat: translator.FormatOpenAIResponse, Affinity: repairChat(t, m, "caller", "next"), Metadata: map[string]any{exec.SelectedAuthCallbackMetadataKey: func(id string) { selected = id }}}
			if _, err := m.Execute(context.Background(), []string{"codex"}, req, opts); err != nil {
				t.Fatal(err)
			}
			want := ids[0]
			if used == 100 {
				// denial makes the first credential ineligible before priority ranking
				want = ids[1]
			}
			if selected != want {
				t.Fatal("event quota did not change next eligibility", selected, want)
			}
		})
	}
}
