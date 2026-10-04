package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	config "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	quota "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

type affinityTestExecutor struct {
	mu               sync.Mutex
	calls            []string
	status           int
	refreshPrincipal string
	refreshCalls     int
	bootstrap        bool
	stream           chan exec.StreamChunk
	entered          chan struct{}
	unblock          chan struct{}
	prepare          func(*Auth) *Auth
	usage            func(context.Context, *Auth, *http.Request) (*http.Response, error)
}

func (e *affinityTestExecutor) Identifier() string           { return "codex" }
func (e *affinityTestExecutor) LocalBuiltinProvider() string { return "codex" }
func (e *affinityTestExecutor) record(ctx context.Context, a *Auth, o exec.Options) error {
	if err := exec.CheckCredentialFence(ctx); err != nil {
		return err
	}
	for key := range o.Headers {
		if affinity.IsLocalHeader(key) {
			return errors.New("local header reached upstream")
		}
	}
	e.mu.Lock()
	e.calls = append(e.calls, a.ID)
	status := e.status
	if status == 401 && e.refreshCalls > 0 && e.refreshPrincipal != "" {
		status = 0
	}
	entered, unblock := e.entered, e.unblock
	e.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if unblock != nil {
		select {
		case <-unblock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if status != 0 {
		return &Error{HTTPStatus: status, Message: "fake upstream failure"}
	}
	return nil
}
func (e *affinityTestExecutor) Execute(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (exec.Response, error) {
	if err := e.record(ctx, a, o); err != nil {
		return exec.Response{}, err
	}
	return exec.Response{Payload: []byte(a.ID)}, nil
}
func (e *affinityTestExecutor) CountTokens(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (exec.Response, error) {
	return e.Execute(ctx, a, r, o)
}
func (e *affinityTestExecutor) ExecuteStream(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (*exec.StreamResult, error) {
	if err := e.record(ctx, a, o); err != nil {
		e.mu.Lock()
		bootstrap := e.bootstrap
		e.mu.Unlock()
		if !bootstrap {
			return nil, err
		}
		ch := make(chan exec.StreamChunk, 1)
		ch <- exec.StreamChunk{Err: err}
		close(ch)
		return &exec.StreamResult{Chunks: ch}, nil
	}
	e.mu.Lock()
	ch := e.stream
	e.mu.Unlock()
	if ch == nil {
		ch = make(chan exec.StreamChunk, 1)
		ch <- exec.StreamChunk{Payload: []byte("ok")}
		close(ch)
	}
	return &exec.StreamResult{Chunks: ch}, nil
}
func (e *affinityTestExecutor) Refresh(ctx context.Context, a *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	principal := e.refreshPrincipal
	e.mu.Unlock()
	a = a.Clone()
	a.Metadata["access_token"] = "refreshed-test-token"
	if principal != "" {
		a.Metadata["account_id"] = principal
		a.Metadata["oauth_provenance"], _ = affinity.AcquiredOAuthProvenance("codex", principal, "refreshed-test-token")
	}
	return a, nil
}
func (e *affinityTestExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	if e.usage == nil {
		return nil, errors.New("fake usage unavailable")
	}
	return e.usage(ctx, a, r)
}
func (e *affinityTestExecutor) ShouldPrepareRequestAuth(*Auth) bool { return e.prepare != nil }
func (e *affinityTestExecutor) PrepareRequestAuth(ctx context.Context, a *Auth) (*Auth, error) {
	return e.prepare(a.Clone()), nil
}
func (e *affinityTestExecutor) IDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func affinityFixture(t *testing.T) (*Manager, *affinityTestExecutor, *config.Config, string, []string) {
	t.Helper()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	e := &affinityTestExecutor{}
	m.RegisterExecutor(e)
	cfg := &config.Config{Routing: config.RoutingConfig{QuotaAwareChatAffinity: &config.QuotaAffinityConfig{Namespace: "test", StateDir: filepath.Join(t.TempDir(), "bindings")}}}
	if err := m.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	m.SetConfig(cfg)
	m.SetRetryConfig(3, time.Second, 0)
	t.Cleanup(func() {
		if err := m.CloseQuotaAffinity(); err != nil {
			t.Error(err)
		}
	})
	model := "affinity-" + strings.ReplaceAll(t.Name(), "/", "-")
	ids := []string{t.Name() + "-a", t.Name() + "-b"}
	for i, id := range ids {
		a := &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "fake-key-" + fmt.Sprint(i), "base_url": "https://fake.invalid"}}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}, {ID: model + "-other"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m, e, cfg, model, ids
}
func affinityRequest(t *testing.T, m *Manager, caller, id string, intent affinity.Intent, h affinity.Handle) affinity.Request {
	t.Helper()
	c, err := affinity.NewCallerScope(caller)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := affinity.NewChatID(id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.NewChatRequest(c, chat, intent, h)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func savedHandle(t *testing.T, h http.Header) affinity.Handle {
	t.Helper()
	handle, err := affinity.ParseHandle(h.Get(affinity.ChatHandleHeader))
	if err != nil {
		t.Fatal(err)
	}
	return handle
}
func expectAffinityReason(t *testing.T, err error, want affinity.UnavailableReason) {
	t.Helper()
	var local *affinity.Error
	if !errors.As(err, &local) || local.Reason() != want {
		t.Fatalf("error = %v, want local reason %v", err, want)
	}
}

func TestQuotaAffinityRetryAuthoritiesNeverDispatchSecondAccount(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream", "bootstrap"} {
		for _, status := range []int{401, 402, 403, 429, 500, 503} {
			t.Run(fmt.Sprintf("%s-%d", kind, status), func(t *testing.T) {
				m, e, _, model, ids := affinityFixture(t)
				req := exec.Request{Model: model}
				start := affinityRequest(t, m, "caller", "chat", affinity.StartIntent, affinity.Handle{})
				first, err := m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: start})
				if err != nil {
					t.Fatal(err)
				}
				continuation := affinityRequest(t, m, "caller", "chat", affinity.ContinueIntent, savedHandle(t, first.Headers))
				e.mu.Lock()
				e.calls = nil
				e.status = status
				if kind == "bootstrap" {
					e.status = 0
					e.stream = make(chan exec.StreamChunk, 1)
					e.stream <- exec.StreamChunk{Err: &Error{HTTPStatus: status, Message: "bootstrap failed"}}
					close(e.stream)
				}
				e.mu.Unlock()
				opts := exec.Options{Affinity: continuation, Headers: http.Header{affinity.ChatHandleHeader: {"must-not-leak"}}}
				switch kind {
				case "execute":
					_, err = m.Execute(context.Background(), []string{"codex", "antigravity"}, req, opts)
				case "count":
					_, err = m.ExecuteCount(context.Background(), []string{"codex"}, req, opts)
				default:
					var s *exec.StreamResult
					s, err = m.ExecuteStream(context.Background(), []string{"codex", "antigravity"}, req, opts)
					if err == nil && s != nil {
						for chunk := range s.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				}
				if err == nil {
					t.Fatal("failure became success")
				}
				for _, id := range e.IDs() {
					if id != ids[0] {
						t.Fatalf("second account dispatched: %v", e.IDs())
					}
				}
				if len(e.IDs()) != 1 {
					t.Fatalf("unexpected attempts %v", e.IDs())
				}
			})
		}
	}
}

func TestQuotaAffinityLifecycleRestartCallerModelAndPin(t *testing.T) {
	m, e, cfg, model, ids := affinityFixture(t)
	req := exec.Request{Model: model}
	start := affinityRequest(t, m, "caller", "same-chat", affinity.StartIntent, affinity.Handle{})
	resp, err := m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: start})
	if err != nil {
		t.Fatal(err)
	}
	handle := savedHandle(t, resp.Headers)
	replay, err := m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: start})
	if err != nil || savedHandle(t, replay.Headers) != handle {
		t.Fatalf("idempotent start: %v", err)
	}
	if err = m.CloseQuotaAffinity(); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(nil, &FillFirstSelector{}, nil)
	restored.RegisterExecutor(e)
	if err = restored.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.CloseQuotaAffinity() }()
	for _, a := range m.List() {
		if _, err = restored.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	turn := affinityRequest(t, restored, "caller", "same-chat", affinity.ContinueIntent, handle)
	_, err = restored.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model + "-other"}, exec.Options{Affinity: turn})
	if err != nil {
		t.Fatal(err)
	}
	isolated := affinityRequest(t, restored, "other-caller", "same-chat", affinity.ContinueIntent, handle)
	_, err = restored.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: isolated})
	expectAffinityReason(t, err, affinity.BindingMissing)
	_, err = restored.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: turn, Metadata: map[string]any{exec.PinnedAuthMetadataKey: ids[1]}})
	expectAffinityReason(t, err, affinity.BindingConflict)
	_, err = restored.Execute(context.Background(), []string{"codex"}, exec.Request{Model: "unsupported-model"}, exec.Options{Affinity: turn})
	expectAffinityReason(t, err, affinity.ModelUnsupported)
	for _, id := range e.IDs() {
		if id != ids[0] {
			t.Fatalf("account changed %v", e.IDs())
		}
	}
}

func TestQuotaAffinityBusyConcurrentStartCountAndCancel(t *testing.T) {
	m, e, _, model, _ := affinityFixture(t)
	e.entered = make(chan struct{}, 1)
	e.unblock = make(chan struct{})
	req := exec.Request{Model: model}
	start := affinityRequest(t, m, "caller", "chat", affinity.StartIntent, affinity.Handle{})
	_, err := m.ExecuteCount(context.Background(), []string{"codex"}, req, exec.Options{Affinity: start})
	expectAffinityReason(t, err, affinity.BindingMissing)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.Execute(ctx, []string{"codex"}, req, exec.Options{Affinity: start}); done <- err }()
	<-e.entered
	_, err = m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: start})
	expectAffinityReason(t, err, affinity.Busy)
	cancel()
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("cancel not propagated")
	}
	e.mu.Lock()
	e.unblock = nil
	e.entered = nil
	e.mu.Unlock()
	first, err := m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: start})
	if err != nil {
		t.Fatal(err)
	}
	turn := affinityRequest(t, m, "caller", "chat", affinity.ContinueIntent, savedHandle(t, first.Headers))
	e.stream = make(chan exec.StreamChunk, 1)
	e.stream <- exec.StreamChunk{Payload: []byte("first")}
	ctx, cancel = context.WithCancel(context.Background())
	s, err := m.ExecuteStream(ctx, []string{"codex"}, req, exec.Options{Affinity: turn})
	if err != nil {
		t.Fatal(err)
	}
	<-s.Chunks
	_, err = m.ExecuteCount(context.Background(), []string{"codex"}, req, exec.Options{Affinity: turn})
	expectAffinityReason(t, err, affinity.Busy)
	cancel()
	for range s.Chunks {
	}
	close(e.stream)
	_, err = m.ExecuteCount(context.Background(), []string{"codex"}, req, exec.Options{Affinity: turn})
	if err != nil {
		t.Fatal(err)
	}
}

func TestQuotaAffinityUnavailableBindingIsNeverCold(t *testing.T) {
	for _, mutation := range []string{"delete", "disable", "key", "origin", "principal", "zero-weight", "quota"} {
		t.Run(mutation, func(t *testing.T) {
			m, e, _, model, ids := affinityFixture(t)
			req := exec.Request{Model: model}
			start := affinityRequest(t, m, "caller", "chat", affinity.StartIntent, affinity.Handle{})
			resp, err := m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: start})
			if err != nil {
				t.Fatal(err)
			}
			turn := affinityRequest(t, m, "caller", "chat", affinity.ContinueIntent, savedHandle(t, resp.Headers))
			a, _ := m.GetByID(ids[0])
			want := affinity.CredentialChanged
			switch mutation {
			case "delete":
				m.Remove(context.Background(), a.ID)
				want = affinity.CredentialMissing
			case "disable":
				a.Disabled = true
				want = affinity.CredentialDisabled
			case "zero-weight":
				a.Attributes[AttributeWeight] = "0"
				want = affinity.CredentialDisabled
			case "key":
				a.Attributes["api_key"] = "replacement-test-key"
			case "origin":
				a.Attributes["base_url"] = "https://different.invalid"
			case "principal":
				a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
				a.Metadata = map[string]any{"account_id": "replacement", "oauth_provenance": testProvenance(t, "replacement", "fake")}
			case "quota":
				a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
				want = affinity.CredentialCooldown
			}
			if mutation != "delete" {
				if _, err = m.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			for _, r := range []affinity.Request{turn, start} {
				_, err = m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: r})
				expectAffinityReason(t, err, want)
			}
			if len(e.IDs()) != 1 {
				t.Fatalf("unavailable looked cold: %v", e.IDs())
			}
		})
	}
}

func TestQuotaAffinityRefreshAndPreparationLineageFence(t *testing.T) {
	for _, kind := range []string{"execute", "stream", "bootstrap", "count", "prepare"} {
		for _, same := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s-same-%v", kind, same), func(t *testing.T) {
				m, e, _, model, ids := affinityFixture(t)
				a, _ := m.GetByID(ids[0])
				a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
				a.Metadata = map[string]any{"account_id": "principal", "oauth_provenance": testProvenance(t, "principal", "fake-old"), "access_token": "fake-old", "refresh_token": "fake-refresh", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}
				if _, err := m.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
				e.status = 401
				e.bootstrap = kind == "bootstrap"
				e.refreshPrincipal = "principal"
				if !same {
					e.refreshPrincipal = "replacement"
				}
				if kind == "prepare" {
					e.status = 0
					e.prepare = func(a *Auth) *Auth {
						if !same {
							a.Metadata["account_id"] = "replacement"
							a.Metadata["oauth_provenance"], _ = affinity.AcquiredOAuthProvenance("codex", "replacement", a.Metadata["access_token"].(string))
						}
						return a
					}
				}
				r := affinityRequest(t, m, "caller", "chat", affinity.StartIntent, affinity.Handle{})
				req := exec.Request{Model: model}
				var err error
				switch kind {
				case "stream", "bootstrap":
					var s *exec.StreamResult
					s, err = m.ExecuteStream(context.Background(), []string{"codex"}, req, exec.Options{Affinity: r})
					if err == nil {
						for range s.Chunks {
						}
					}
				case "count":
					e.status = 0
					first, errStart := m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: r})
					if errStart != nil {
						err = errStart
					} else {
						r = affinityRequest(t, m, "caller", "chat", affinity.ContinueIntent, savedHandle(t, first.Headers))
						e.status = 401
						_, err = m.ExecuteCount(context.Background(), []string{"codex"}, req, exec.Options{Affinity: r})
					}
				default:
					_, err = m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: r})
				}
				if same && err != nil {
					t.Fatal(err)
				}
				if !same {
					expectAffinityReason(t, err, affinity.CredentialChanged)
				}
				for _, id := range e.IDs() {
					if id != ids[0] {
						t.Fatalf("second account %v", e.IDs())
					}
				}
				maxDispatches := 1
				if kind == "count" {
					maxDispatches = 2
				}
				if !same && len(e.IDs()) > maxDispatches {
					t.Fatalf("replacement retried %v", e.IDs())
				}
			})
		}
	}
}

func TestQuotaAffinityHooksRejectedBeforeBindingOrDispatch(t *testing.T) {
	m, e, _, model, _ := affinityFixture(t)
	r := affinityRequest(t, m, "caller", "chat", affinity.StartIntent, affinity.Handle{})
	called := false
	opts := exec.Options{Affinity: r, RequestAfterAuthInterceptor: func(context.Context, exec.RequestAfterAuthInterceptRequest) exec.RequestAfterAuthInterceptResponse {
		called = true
		return exec.RequestAfterAuthInterceptResponse{Terminate: true, StatusCode: 418}
	}}
	_, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, opts)
	expectAffinityReason(t, err, affinity.PrefixTransformUnsupported)
	_, found, err := m.affinityRuntime.store.Lookup(context.Background(), r.Key())
	if err != nil || found || called || len(e.IDs()) != 0 {
		t.Fatal("hook ran, binding created, or request dispatched", err)
	}
}

func quotaObservation(t *testing.T, lease QuotaLease, received, reset time.Time, amount float64) quota.Snapshot {
	t.Helper()
	scope, _ := quota.SharedCredential("generation")
	period, _ := quota.Rolling(time.Hour)
	remaining, _ := quota.Fraction(amount, quota.ComparisonKey{})
	s, err := quota.NewSnapshot(quota.SnapshotInput{Credential: lease.Credential(), Source: quota.ExistingUsageGET, ObservedAt: received, Windows: []quota.WindowInput{{ID: "primary", Scope: scope, Period: period, ResetAt: &reset, Remaining: remaining}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQuotaAffinityReceiptGenerationCloneAndColdRanking(t *testing.T) {
	m, e, cfg, model, ids := affinityFixture(t)
	now := time.Now().UTC()
	m.affinityRuntime.now = func() time.Time { return now }
	leases := make([]QuotaLease, 2)
	for i, id := range ids {
		lease, err := m.QuotaLease(id)
		if err != nil {
			t.Fatal(err)
		}
		leases[i] = lease
	}
	first := quotaObservation(t, leases[0], now, now.Add(2*time.Hour), .9)
	second := quotaObservation(t, leases[1], now, now.Add(time.Hour), .1)
	if !m.ObserveQuota(leases[0], first) || !m.ObserveQuota(leases[1], second) {
		t.Fatal("observation rejected")
	}
	if m.ObserveQuota(leases[0], quotaObservation(t, leases[0], now.Add(-time.Minute), now.Add(time.Hour), 0)) {
		t.Fatal("old observation overwrote receipt")
	}
	if m.ObserveQuota(leases[0], quotaObservation(t, leases[0], now.Add(time.Second), now.Add(time.Hour), 0)) {
		t.Fatal("future receipt accepted")
	}
	if err := m.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	start := affinityRequest(t, m, "caller", "chat", affinity.StartIntent, affinity.Handle{})
	resp, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: start})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Payload) != ids[1] {
		t.Fatalf("rank winner %s", resp.Payload)
	}
	if m.ObserveQuota(leases[1], quotaObservation(t, leases[1], now, now.Add(time.Hour), 0)) {
		t.Fatal("generation fence accepted old response")
	}
	freshLease, err := m.QuotaLease(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if !m.ObserveQuota(freshLease, quotaObservation(t, freshLease, now, now.Add(time.Hour), 0)) {
		t.Fatal("new denial rejected")
	}
	turn := affinityRequest(t, m, "caller", "chat", affinity.ContinueIntent, savedHandle(t, resp.Headers))
	_, err = m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: turn})
	expectAffinityReason(t, err, affinity.QuotaDenied)
	if len(e.IDs()) != 1 {
		t.Fatal("denial rerouted")
	}
	a, _ := m.GetByID(ids[0])
	a.Metadata = map[string]any{"unrelated": "copy"}
	if _, err = m.Update(context.Background(), a.Clone()); err != nil {
		t.Fatal(err)
	}
	if m.affinityView(leases[0].ref, model).Denied() {
		t.Fatal("clone changed quota")
	}
	now = now.Add(6 * time.Minute)
	if _, ok := m.affinityView(leases[0].ref, model).NextReset(); ok {
		t.Fatal("stale view ranked")
	}
}

func TestQuotaAffinityPollingBoundedCancellationFailureAndFences(t *testing.T) {
	m, e, _, _, ids := affinityFixture(t)
	for _, id := range ids {
		a, _ := m.GetByID(id)
		a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
		a.Metadata = map[string]any{"account_id": id, "oauth_provenance": testProvenance(t, id, "fake"), "access_token": "fake"}
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	var running, maximum atomic.Int32
	entered := make(chan string, 2)
	release := make(chan struct{})
	e.usage = func(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		if r.Method != "GET" || r.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
			return nil, errors.New("wrong usage request")
		}
		entered <- a.ID
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":3600,"reset_after_seconds":3600}}}`))}, nil
	}
	m.affinityRuntime.settings.PollConcurrency = 1
	done := make(chan struct{})
	go func() { m.PollQuotaOnce(context.Background()); close(done) }()
	id := <-entered
	a, _ := m.GetByID(id)
	a.Metadata["account_id"] = "replacement"
	a.Metadata["oauth_provenance"], _ = affinity.AcquiredOAuthProvenance("codex", "replacement", a.Metadata["access_token"].(string))
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if maximum.Load() != 1 {
		t.Fatal("poll exceeded concurrency")
	}
	m.affinityMu.Lock()
	_, accepted := m.affinityRuntime.observations[id]
	m.affinityMu.Unlock()
	if accepted {
		t.Fatal("stale poll accepted replacement")
	}
	lease, _ := m.QuotaLease(ids[1])
	before := m.affinityView(lease.ref, "")
	e.usage = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
		return nil, errors.New("failure with fake secret")
	}
	m.PollQuotaOnce(context.Background())
	after := m.affinityView(lease.ref, "")
	if before.Freshness() != after.Freshness() {
		t.Fatal("failed poll changed freshness")
	}
	enteredCancel := make(chan struct{}, 2)
	e.usage = func(ctx context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
		enteredCancel <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { m.PollQuotaOnce(ctx); close(enteredCancel) }()
	<-enteredCancel
	cancel()
	for range enteredCancel {
	}
}

func TestQuotaAffinityStoreFailureRetentionAndUnsafeReload(t *testing.T) {
	m, e, cfg, model, _ := affinityFixture(t)
	now := time.Now().UTC()
	m.affinityRuntime.now = func() time.Time { return now }
	r := affinityRequest(t, m, "caller", "chat", affinity.StartIntent, affinity.Handle{})
	resp, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: r})
	if err != nil {
		t.Fatal(err)
	}
	changed := *cfg
	changed.Routing = cfg.Routing
	profile := *cfg.Routing.QuotaAwareChatAffinity
	profile.Namespace = "new"
	changed.Routing.QuotaAwareChatAffinity = &profile
	if m.ValidateQuotaAffinityReload(&changed) == nil {
		t.Fatal("unsafe reload accepted")
	}
	if m.ValidateQuotaAffinityReload(&config.Config{}) == nil {
		t.Fatal("profile downgrade accepted")
	}
	now = now.Add(31 * 24 * time.Hour)
	turn := affinityRequest(t, m, "caller", "chat", affinity.ContinueIntent, savedHandle(t, resp.Headers))
	_, err = m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: turn})
	expectAffinityReason(t, err, affinity.BindingExpired)
	if err = m.CloseQuotaAffinity(); err != nil {
		t.Fatal(err)
	}
	_, err = m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: r})
	expectAffinityReason(t, err, affinity.StoreUnavailable)
	if len(e.IDs()) != 1 {
		t.Fatal("failed store dispatched")
	}
}

func testProvenance(t *testing.T, id, token string) affinity.OAuthProvenance {
	t.Helper()
	p, err := affinity.AcquiredOAuthProvenance("codex", id, token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQuotaAffinityInitialPeriodicPollAndPassiveReceipt(t *testing.T) {
	m, e, _, model, ids := affinityFixture(t)
	a, _ := m.GetByID(ids[0])
	a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	a.Metadata = map[string]any{"account_id": "principal", "access_token": "fake", "oauth_provenance": testProvenance(t, "principal", "fake")}
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	calls := make(chan struct{}, 4)
	e.usage = func(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
		calls <- struct{}{}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":3600,"reset_after_seconds":3600}}}`))}, nil
	}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.runQuotaPoller(ctx, ticks); close(done) }()
	<-calls
	ticks <- time.Now()
	<-calls
	cancel()
	<-done
	view, err := m.QuotaView(ids[0], model)
	if err != nil || view.Knowledge() != quota.Known {
		t.Fatalf("poll observation %v", err)
	}
	lease, _ := m.QuotaLease(ids[0])
	attempt := m.quotaAttemptContext(context.Background(), lease)
	exec.ObserveQuotaEvent(attempt, []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":60,"reset_after_seconds":3600}}}`))
	view, err = m.QuotaView(ids[0], model)
	if err != nil || !view.Denied() {
		t.Fatal("passive event receipt not ingested")
	}
}

func TestQuotaAffinityOAuthTokenReplacementRequiresAcquisitionProof(t *testing.T) {
	m, _, _, _, ids := affinityFixture(t)
	a, _ := m.GetByID(ids[0])
	a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	a.Metadata = map[string]any{"account_id": "principal", "access_token": "fake-old", "oauth_provenance": testProvenance(t, "principal", "fake-old")}
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	lease, err := m.QuotaLease(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	a.Metadata["access_token"] = "fake-setup-token"
	if _, err = m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err = m.QuotaLease(ids[0]); err == nil {
		t.Fatal("replacement inherited profile proof")
	}
	a.Metadata["oauth_provenance"] = testProvenance(t, "principal", "fake-setup-token")
	if _, err = m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	refreshed, err := m.QuotaLease(ids[0])
	if err != nil || refreshed.Credential() != lease.Credential() {
		t.Fatal("same acquired principal refresh rejected")
	}
	delete(a.Metadata, "oauth_provenance")
	if _, err = m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err = m.QuotaLease(ids[0]); err == nil {
		t.Fatal("synthetic identity accepted")
	}
}

func TestQuotaAffinityMultiWindowReceiptsAndPriority(t *testing.T) {
	m, _, _, model, ids := affinityFixture(t)
	now := time.Now().UTC()
	m.affinityRuntime.now = func() time.Time { return now }
	scope, _ := quota.SharedCredential("generation")
	period, _ := quota.Rolling(time.Hour)
	group, _ := quota.NewComparisonKey("same-test-entitlement")
	observe := func(id, window string, amount float64, reset time.Time) {
		t.Helper()
		lease, err := m.QuotaLease(id)
		if err != nil {
			t.Fatal(err)
		}
		remaining, _ := quota.Fraction(amount, group)
		s, err := quota.NewSnapshot(quota.SnapshotInput{Credential: lease.Credential(), Source: quota.PassiveHTTP, ObservedAt: now, Windows: []quota.WindowInput{{ID: window, Scope: scope, Period: period, ResetAt: &reset, Remaining: remaining}}})
		if err != nil || !m.ObserveQuota(lease, s) {
			t.Fatalf("observation rejected: %v", err)
		}
	}
	choose := func(chat string) string {
		t.Helper()
		r := affinityRequest(t, m, "caller", chat, affinity.StartIntent, affinity.Handle{})
		result, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: r})
		if err != nil {
			t.Fatal(err)
		}
		return string(result.Payload)
	}
	reset := now.Add(time.Hour)
	observe(ids[0], "primary", .1, reset)
	observe(ids[1], "primary", .9, reset)
	if got := choose("tie"); got != ids[1] {
		t.Fatalf("allowance tie winner = %s", got)
	}
	a, _ := m.GetByID(ids[0])
	a.Attributes["priority"] = "10"
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if got := choose("priority"); got != ids[0] {
		t.Fatalf("priority tier winner = %s", got)
	}
	// a separate fresh exhausted window blocks the positive primary window
	observe(ids[0], "secondary", 0, now.Add(2*time.Hour))
	if got := choose("multiple-windows"); got != ids[1] {
		t.Fatalf("denied tier admitted = %s", got)
	}
	now = now.Add(4 * time.Minute)
	observe(ids[0], "primary", .5, now.Add(30*time.Minute))
	lease, _ := m.QuotaLease(ids[0])
	if !m.affinityView(lease.ref, model).Denied() {
		t.Fatal("partial positive observation cleared another window denial")
	}
	now = now.Add(2 * time.Minute)
	lease, _ = m.QuotaLease(ids[0])
	if m.affinityView(lease.ref, model).Denied() {
		t.Fatal("partial update made old denial fresh")
	}
	if got := choose("stale-other-window"); got != ids[0] {
		t.Fatalf("fresh primary was not used = %s", got)
	}
	now = now.Add(6 * time.Minute)
	if got := choose("unknown-fallback"); got != ids[0] {
		t.Fatalf("unknown fallback left highest tier = %s", got)
	}
}

func TestQuotaAffinityLocalErrorsTakePrecedence(t *testing.T) {
	local := affinity.NewError(affinity.CredentialChanged, time.Time{})
	err := preferredExecutionAttemptError(local, &Error{HTTPStatus: 401, Message: "old upstream failure"})
	expectAffinityReason(t, err, affinity.CredentialChanged)
}

func TestQuotaAffinityAuthReloadKeepsBindingBusyAndFencesObservations(t *testing.T) {
	m, e, cfg, model, ids := affinityFixture(t)
	req := exec.Request{Model: model}
	start := affinityRequest(t, m, "caller", "chat", affinity.StartIntent, affinity.Handle{})
	first, err := m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: start})
	if err != nil {
		t.Fatal(err)
	}
	turn := affinityRequest(t, m, "caller", "chat", affinity.ContinueIntent, savedHandle(t, first.Headers))
	lease, err := m.QuotaLease(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if !m.ObserveQuota(lease, quotaObservation(t, lease, now, now.Add(time.Hour), .5)) {
		t.Fatal("observation rejected")
	}
	store := newMemoryAuthTestStore()
	for _, a := range m.List() {
		if _, err := store.Save(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	m.SetStore(store)
	e.stream = make(chan exec.StreamChunk, 1)
	e.stream <- exec.StreamChunk{Payload: []byte("first")}
	stream, err := m.ExecuteStream(context.Background(), []string{"codex"}, req, exec.Options{Affinity: turn})
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Chunks
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.ConfigureQuotaAffinity(cfg); err != nil {
		t.Fatal(err)
	}
	if m.ObserveQuota(lease, quotaObservation(t, lease, now, now.Add(time.Hour), 0)) {
		t.Fatal("old pre-reload lease accepted")
	}
	view, err := m.QuotaView(ids[0], model)
	if err != nil || view.Knowledge() != quota.Known || view.Denied() {
		t.Fatal("reload lost or replaced valid observations")
	}
	_, err = m.ExecuteCount(context.Background(), []string{"codex"}, req, exec.Options{Affinity: turn})
	expectAffinityReason(t, err, affinity.Busy)
	close(e.stream)
	for range stream.Chunks {
	}
	if _, err := m.ExecuteCount(context.Background(), []string{"codex"}, req, exec.Options{Affinity: turn}); err != nil {
		t.Fatal(err)
	}
	a, _ := m.GetByID(ids[0])
	a.Attributes["api_key"] = "fake-replacement-key"
	if _, err := store.Save(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	view, err = m.QuotaView(ids[0], model)
	if err != nil || view.Knowledge() != quota.Unknown {
		t.Fatal("replacement reused old quota observations")
	}
	_, err = m.Execute(context.Background(), []string{"codex"}, req, exec.Options{Affinity: turn})
	expectAffinityReason(t, err, affinity.CredentialChanged)
	for _, id := range e.IDs() {
		if id != ids[0] {
			t.Fatal("reload dispatched another account")
		}
	}
}
