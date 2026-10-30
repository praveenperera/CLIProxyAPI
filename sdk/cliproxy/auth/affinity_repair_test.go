package auth

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type observerExecution struct {
	*affinityTestExecutor
	wire chan string
}

func (e *observerExecution) Execute(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (exec.Response, error) {
	if e.wire != nil {
		e.wire <- a.Attributes["api_key"]
	}
	resp, err := e.affinityTestExecutor.Execute(ctx, a, r, o)
	if err == nil {
		exec.ObserveQuotaEvent(ctx, []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":60,"reset_after_seconds":3600}}}`))
	}
	return resp, err
}
func TestQuotaObserverRejectsReplacedExecutionSnapshot(t *testing.T) {
	m, e, _, model, ids := affinityFixture(t)
	wire := make(chan string, 2)
	m.RegisterExecutor(&observerExecution{e, wire})
	selected := make(chan struct{})
	replaced := make(chan struct{})
	done := make(chan error, 1)
	opts := exec.Options{Metadata: map[string]any{exec.PinnedAuthMetadataKey: ids[0], exec.SelectedAuthCallbackMetadataKey: func(string) { close(selected); <-replaced }}}
	go func() {
		_, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, opts)
		done <- err
	}()
	<-selected // the real legacy SDK execution already selected its auth snapshot
	old, _ := m.GetByID(ids[0])
	replacement := old.Clone()
	replacement.Attributes["api_key"] = "replacement-key"
	if _, err := m.Update(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	close(replaced)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := <-wire; got != old.Attributes["api_key"] {
		t.Fatal("test did not execute selected old credential", got)
	}
	view, err := m.QuotaView(ids[0], model)
	if err != nil || view.Denied() {
		t.Fatal("old execution exhausted replacement", err)
	}
	// stale setup must also remove a previously attached observer
	current, _ := m.GetByID(ids[0])
	inherited := m.observationContextForAuth(context.Background(), current)
	rejected := m.observationContextForAuth(inherited, old)
	exec.ObserveQuotaEvent(rejected, []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":60,"reset_after_seconds":3600}}}`))
	view, _ = m.QuotaView(ids[0], model)
	if view.Denied() {
		t.Fatal("stale attempt inherited another snapshot's observer")
	}
	// current zero-affinity SDK observations still feed strict cold ranking
	if _, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Metadata: map[string]any{exec.PinnedAuthMetadataKey: ids[0]}}); err != nil {
		t.Fatal(err)
	}
	<-wire
	request := affinityRequest(t, m, "caller", "new", affinity.StartIntent, affinity.Handle{})
	if _, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{Affinity: request}); err != nil {
		t.Fatal(err)
	}
	<-wire
	if got := e.IDs(); len(got) != 3 || got[2] != ids[1] {
		t.Fatal("legacy observation did not feed strict ranking", got)
	}
}

type cancelPollBody struct {
	ctx     context.Context
	entered chan<- struct{}
	once    sync.Once
}

func (b *cancelPollBody) Read([]byte) (int, error) {
	b.once.Do(func() { b.entered <- struct{}{} })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*cancelPollBody) Close() error { return nil }

func TestQuotaPollEstablishedBodyNoDeadlineAndOverlappingLimits(t *testing.T) {
	m, e, _, _, ids := affinityFixture(t)
	for _, id := range ids {
		a, _ := m.GetByID(id)
		a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
		a.Metadata = map[string]any{"account_id": id, "access_token": "fake", "oauth_provenance": testProvenance(t, id, "fake")}
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	m.affinityRuntime.settings.PollConcurrency = 1
	m.affinityRuntime.pollSlots = make(chan struct{}, 1)
	entered := make(chan struct{}, 8)
	var calls atomic.Int32
	e.usage = func(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("post-connection poll deadline")
		}
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: &cancelPollBody{ctx: ctx, entered: entered}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.PollQuotaOnce(ctx) }()
	<-entered // first established response holds the global worker slot
	starts := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-starts; m.PollQuotaOnce(ctx) }()
	}
	close(starts)
	// use the per-credential map as a barrier: another ID is queued at the same global slot
	for {
		m.affinityMu.Lock()
		n := len(m.affinityRuntime.polling)
		m.affinityMu.Unlock()
		if n == 2 {
			break
		}
		// scheduler yield, not a wall-clock wait
		runtime.Gosched()
	}
	if calls.Load() != 1 {
		t.Fatal("overlapping passes exceeded global worker limit")
	}
	cancel()
	wg.Wait()
	m.affinityMu.Lock()
	active := len(m.affinityRuntime.polling)
	m.affinityMu.Unlock()
	if active != 0 || len(m.affinityRuntime.pollSlots) != 0 {
		t.Fatal("cancellation did not join workers")
	}
	// a completed pass can use the same credentials again
	e.usage = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
	m.PollQuotaOnce(context.Background())
}

func TestAffinityWireCredentialHeaderCaseAndAmbiguity(t *testing.T) {
	a := &Auth{Provider: "codex", Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, "base_url": "https://fake.invalid"}, Metadata: map[string]any{"access_token": "fake", "account_id": "account"}}
	r, _ := http.NewRequest("GET", "wss://fake.invalid/responses", nil)
	r.Header = http.Header{"Authorization": {"Bearer fake"}, "ChatGPT-Account-ID": {"account"}}
	if err := checkAffinityCredentialRequest(a, r, false); err != nil {
		t.Fatal("case-preserved handshake rejected", err)
	}
	r.Header.Set("Chatgpt-Account-Id", "account")
	if err := checkAffinityCredentialRequest(a, r, false); err == nil {
		t.Fatal("ambiguous account headers accepted")
	}
	delete(r.Header, "Chatgpt-Account-Id")
	r.Header["authorization"] = []string{"Bearer other"}
	if err := checkAffinityCredentialRequest(a, r, false); err == nil {
		t.Fatal("ambiguous credential headers accepted")
	}
}

func TestQuotaPollOverlappingPassesExcludeInFlightCredential(t *testing.T) {
	m, e, _, _, ids := affinityFixture(t)
	a, _ := m.GetByID(ids[0])
	a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	a.Metadata = map[string]any{"account_id": ids[0], "access_token": "fake", "oauth_provenance": testProvenance(t, ids[0], "fake")}
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 4)
	var calls atomic.Int32
	e.usage = func(ctx context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: &cancelPollBody{ctx: ctx, entered: entered}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.PollQuotaOnce(ctx); close(done) }()
	<-entered
	// these complete while the first response stays open; spare global slots cannot duplicate it
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.PollQuotaOnce(ctx) }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("overlapping public passes duplicated the hung credential", calls.Load())
	}
	cancel()
	<-done
}
