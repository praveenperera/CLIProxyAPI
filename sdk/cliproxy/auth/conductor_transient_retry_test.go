package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type transientTestExecutor struct {
	affinityTestExecutor
	calls        []string
	fail         func(string, int) error
	bootstrap    bool
	empty        bool
	afterData    bool
	attempts     []context.Context
	inputPayload [][]byte
}

func (e *transientTestExecutor) Execute(ctx context.Context, auth *Auth, req exec.Request, opts exec.Options) (exec.Response, error) {
	e.calls = append(e.calls, auth.ID)
	e.attempts = append(e.attempts, ctx)
	e.inputPayload = append(e.inputPayload, append([]byte(nil), req.Payload...))
	exec.MarkUpstreamAttempt(ctx)
	if e.fail != nil {
		if err := e.fail(auth.ID, len(e.calls)); err != nil {
			return exec.Response{}, err
		}
	}
	return exec.Response{Payload: []byte(auth.ID)}, nil
}

func (e *transientTestExecutor) CountTokens(ctx context.Context, auth *Auth, req exec.Request, opts exec.Options) (exec.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *transientTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	resp, err := e.Execute(ctx, auth, req, opts)
	if err != nil && !e.bootstrap {
		return nil, err
	}
	ch := make(chan exec.StreamChunk, 3)
	ch <- exec.StreamChunk{}
	if err == nil {
		ch <- exec.StreamChunk{Payload: resp.Payload}
	} else if e.afterData {
		ch <- exec.StreamChunk{Payload: []byte("data")}
	}
	if err != nil && !e.empty {
		ch <- exec.StreamChunk{Err: err}
	}
	close(ch)
	return &exec.StreamResult{Chunks: ch, Headers: http.Header{"X-Upstream": {auth.ID}}}, nil
}

func transientFixture(t *testing.T) (*Manager, *transientTestExecutor, string, []string, *[]time.Duration) {
	t.Helper()
	m := NewManager(nil, NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}}), nil)
	m.SetTransientRetryConfig(2)
	var waits []time.Duration
	m.transientRetryWait = func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return ctx.Err()
	}
	e := &transientTestExecutor{}
	m.RegisterExecutor(e)
	model := "transient-" + t.Name()
	ids := []string{model + "-a", model + "-b"}
	for _, id := range ids {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	t.Cleanup(func() { m.SetSelector(&FillFirstSelector{}) })
	return m, e, model, ids, &waits
}

func transientOptions() exec.Options {
	return exec.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {"retry-session"}}}
}

func runTransientPath(ctx context.Context, m *Manager, model, path string) (string, error) {
	req := exec.Request{Model: model, Payload: []byte(`{"original":true}`)}
	opts := transientOptions()
	switch path {
	case "stream":
		result, err := m.ExecuteStream(ctx, []string{"codex"}, req, opts)
		if err != nil {
			return "", err
		}
		var payload []byte
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				return string(payload), chunk.Err
			}
			payload = append(payload, chunk.Payload...)
		}
		return string(payload), nil
	case "count":
		resp, err := m.ExecuteCount(ctx, []string{"codex"}, req, opts)
		return string(resp.Payload), err
	default:
		resp, err := m.Execute(ctx, []string{"codex"}, req, opts)
		return string(resp.Payload), err
	}
}

func TestManagerTransientRetries(t *testing.T) {
	for _, path := range []string{"execute", "count", "stream"} {
		for _, scenario := range []string{"recover", "exhaust", "quota", "disabled", "bootstrap", "empty"} {
			t.Run(path+"/"+scenario, func(t *testing.T) {
				m, e, model, ids, waits := transientFixture(t)
				// bind a session before the error, so a successful retry must preserve it
				if got, err := runTransientPath(context.Background(), m, model, path); err != nil || got != ids[0] {
					t.Fatalf("initial result = %q, %v", got, err)
				}
				hook := &recordingHook{}
				m.hook = hook
				wait := m.transientRetryWait
				m.transientRetryWait = func(ctx context.Context, delay time.Duration) error {
					if result := hook.lastResult.Load(); result != nil {
						t.Fatalf("intermediate attempt was recorded: %+v", result)
					}
					auth, _ := m.GetByID(ids[0])
					if auth.Unavailable || auth.LastError != nil {
						t.Fatalf("intermediate attempt changed credential state: %+v", auth)
					}
					return wait(ctx, delay)
				}
				e.calls = nil
				e.attempts = nil
				e.inputPayload = nil
				e.bootstrap = scenario == "bootstrap" || scenario == "empty"
				e.empty = scenario == "empty"
				if scenario == "disabled" {
					m.SetTransientRetryConfig(0)
				}
				e.fail = func(id string, call int) error {
					if id != ids[0] {
						return nil
					}
					if (scenario == "recover" || scenario == "bootstrap" || scenario == "empty") && call > 2 {
						return nil
					}
					if scenario == "quota" {
						return &Error{HTTPStatus: 429}
					}
					return &Error{HTTPStatus: 529, Message: "overloaded_error"}
				}
				got, err := runTransientPath(context.Background(), m, model, path)
				wantCalls := []string{ids[0], ids[0], ids[0]}
				wantAuth := ids[0]
				wantWaits := []time.Duration{time.Second, 2 * time.Second}
				if scenario == "exhaust" {
					wantCalls = append(wantCalls, ids[1])
					wantAuth = ids[1]
				}
				if scenario == "quota" || scenario == "disabled" {
					wantCalls = []string{ids[0], ids[1]}
					wantAuth, wantWaits = ids[1], nil
				}
				if err != nil || got != wantAuth || !reflect.DeepEqual(e.calls, wantCalls) || !reflect.DeepEqual(*waits, wantWaits) {
					t.Fatalf("result = %q, %v; calls = %v; waits = %v; want %q, %v, %v", got, err, e.calls, *waits, wantAuth, wantCalls, wantWaits)
				}
				for i := 1; i < len(e.attempts); i++ {
					if e.attempts[i] == e.attempts[i-1] {
						t.Fatal("retry reused the upstream attempt context")
					}
				}
				for _, payload := range e.inputPayload {
					if string(payload) != `{"original":true}` {
						t.Fatalf("retry received mutated input: %s", payload)
					}
				}
				auth, _ := m.GetByID(ids[0])
				if wantAuth == ids[0] && (auth.Unavailable || auth.LastError != nil) {
					t.Fatalf("intermediate failure affected auth state: %+v", auth)
				}
				e.fail = nil
				if got, err := runTransientPath(context.Background(), m, model, path); err != nil || got != wantAuth {
					t.Fatalf("session binding = %q, %v; want %s", got, err, wantAuth)
				}
			})
		}
	}
}

func TestManagerTransientRetryCancelBackoff(t *testing.T) {
	for _, path := range []string{"execute", "count", "stream"} {
		t.Run(path, func(t *testing.T) {
			m, e, model, ids, _ := transientFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.fail = func(string, int) error { return &Error{HTTPStatus: 503} }
			m.transientRetryWait = func(ctx context.Context, delay time.Duration) error {
				cancel()
				return waitForTransientRetry(ctx, delay)
			}
			_, err := runTransientPath(ctx, m, model, path)
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(e.calls, []string{ids[0]}) {
				t.Fatalf("result = %v, calls = %v", err, e.calls)
			}
		})
	}
}

func TestManagerTransientRetryDoesNotReplayData(t *testing.T) {
	m, e, model, ids, waits := transientFixture(t)
	e.bootstrap, e.afterData = true, true
	e.fail = func(string, int) error { return &Error{HTTPStatus: 503} }
	got, err := runTransientPath(context.Background(), m, model, "stream")
	if got != "data" || statusCodeFromError(err) != 503 || !reflect.DeepEqual(e.calls, []string{ids[0]}) || len(*waits) != 0 {
		t.Fatalf("result = %q, %v; calls = %v; waits = %v", got, err, e.calls, *waits)
	}
}

func TestTransientRetryClassification(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetTransientRetryConfig(2)
	r := m.newTransientRetry()
	for _, status := range []int{408, 500, 502, 503, 504, 520, 522, 524, 529} {
		if !r.allows(nil, exec.Options{}, &Error{HTTPStatus: status}) {
			t.Errorf("status %d did not allow retry", status)
		}
	}
	for _, err := range []error{io.ErrUnexpectedEOF, dialRefusedError(), &Error{Code: "overloaded_error"}} {
		if !r.allows(nil, exec.Options{}, err) {
			t.Errorf("did not allow retry: %v", err)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, &Error{HTTPStatus: 400}, &Error{HTTPStatus: 401}, &Error{HTTPStatus: 402}, &Error{HTTPStatus: 403}, &Error{HTTPStatus: 404}, &Error{HTTPStatus: 429}, &Error{HTTPStatus: 503, Message: "invalid_grant"}, &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}, wrapRequestStopError(&Error{HTTPStatus: 503})} {
		if r.allows(nil, exec.Options{}, err) {
			t.Errorf("allowed retry: %v", err)
		}
	}
	auth := &Auth{Metadata: map[string]any{"request_scoped_errors": []config.RequestScopedErrorRule{
		{Status: 503, Match: []string{"stop"}, Action: RequestScopedActionStop},
		{Status: 503, Match: []string{"next"}, Action: RequestScopedActionContinue},
	}}}
	if r.allows(auth, exec.Options{}, &Error{HTTPStatus: 503, Message: "stop"}) {
		t.Fatal("retried a configured stop action")
	}
	if r.allows(auth, exec.Options{}, &Error{HTTPStatus: 503, Message: "next"}) {
		t.Fatal("retried a configured continue action")
	}
}

func TestManagerTransientRetryKeepsBootstrapErrorPath(t *testing.T) {
	m, e, model, ids, _ := transientFixture(t)
	e.bootstrap = true
	e.fail = func(string, int) error { return &Error{HTTPStatus: 400, Message: "bad request"} }
	_, err := runTransientPath(context.Background(), m, model, "stream")
	// a request fault at bootstrap ends on the bootstrap path, which returns the upstream error
	// itself rather than the stream-start failure wrapper
	var bootstrap *streamBootstrapError
	if statusCodeFromError(err) != 400 || errors.As(err, &bootstrap) || !reflect.DeepEqual(e.calls, []string{ids[0]}) {
		t.Fatalf("err = %T %v, calls = %v; want the raw 400 from %s only", err, err, e.calls, ids[0])
	}
}

func TestManagerTransientRetrySharesBudgetWithRefresh(t *testing.T) {
	for _, path := range []string{"execute", "count", "stream"} {
		t.Run(path, func(t *testing.T) {
			m, e, model, ids, waits := transientFixture(t)
			auth, _ := m.GetByID(ids[0])
			auth.Metadata = map[string]any{"access_token": "old", "refresh_token": "refresh"}
			if _, err := m.Update(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			e.bootstrap = path == "stream"
			e.fail = func(id string, call int) error {
				if id != ids[0] {
					return nil
				}
				if call == 2 {
					return &Error{HTTPStatus: 401}
				}
				return &Error{HTTPStatus: 503}
			}
			got, err := runTransientPath(context.Background(), m, model, path)
			if err != nil || got != ids[1] || len(e.calls) != 5 || len(*waits) != 2 || e.refreshCalls != 1 {
				t.Fatalf("result = %q, %v; calls = %v; waits = %v; refreshes = %d", got, err, e.calls, *waits, e.refreshCalls)
			}
		})
	}
}

func TestManagerTransientRetryRounds(t *testing.T) {
	m, e, model, ids, waits := transientFixture(t)
	m.SetRetryConfig(1, 0, 0)
	e.fail = func(id string, call int) error {
		if call > 6 {
			return nil
		}
		return fmt.Errorf("transport: %w", io.ErrUnexpectedEOF)
	}
	got, err := runTransientPath(context.Background(), m, model, "execute")
	if err != nil || got != ids[1] || len(e.calls) != 7 || len(*waits) != 4 {
		t.Fatalf("result = %q, %v; calls = %v; waits = %v", got, err, e.calls, *waits)
	}
}
