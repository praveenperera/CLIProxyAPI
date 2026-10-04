package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	runtimequota "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	domain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type affinityExecutor struct {
	manager  *Manager
	delegate ProviderExecutor
	ref      affinity.CredentialRef
	model    string
	opts     exec.Options
}

func (e *affinityExecutor) Identifier() string { return e.delegate.Identifier() }
func (e *affinityExecutor) check(ctx context.Context, a *Auth) (context.Context, error) {
	current, err := e.manager.credentialRef(a)
	if err != nil || current != e.ref {
		return ctx, affinity.NewError(affinity.CredentialChanged, time.Time{})
	}
	if _, _, err = e.manager.validateAffinityAuth(ctx, e.ref, e.model, e.opts); err != nil {
		return ctx, err
	}
	lease := QuotaLease{e.ref, a.RegistrationEpoch, a.Generation}
	ctx = exec.WithTransportIdentity(ctx, e.ref, e.opts.Affinity.Key())
	ctx = exec.WithCredentialFence(ctx, func(request *http.Request) error {
		if _, _, err := e.manager.validateAffinityAuth(ctx, e.ref, e.model, e.opts); err != nil {
			return err
		}
		return checkAffinityCredentialRequest(a, request, false)
	})
	return e.manager.quotaAttemptContext(ctx, lease), nil
}
func (m *Manager) quotaAttemptContext(ctx context.Context, lease QuotaLease) context.Context {
	return exec.WithQuotaObserver(ctx, func(headers http.Header, payload []byte, received time.Time) {
		var s domain.Snapshot
		var err error
		if payload != nil {
			s, err = runtimequota.ParseCodexEvent(lease.ref, payload, received)
		} else {
			s, err = runtimequota.ParseHeaders(lease.ref, headers, received)
		}
		if err == nil {
			m.ObserveQuota(lease, s)
		}
	})
}
func (e *affinityExecutor) Execute(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (exec.Response, error) {
	ctx, err := e.check(ctx, a)
	if err != nil {
		return exec.Response{}, err
	}
	o.Headers = affinity.StripHeaders(o.Headers)
	if e.ref.Provider() == "claude" {
		o.NativeHistory = exec.NativeHistoryPreserve
		if errPreflight := e.manager.preflightNativeHistory(ctx, e.delegate, a, r, o); errPreflight != nil {
			return exec.Response{}, errPreflight
		}
	}
	return e.delegate.Execute(ctx, a, r, o)
}
func (e *affinityExecutor) CountTokens(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (exec.Response, error) {
	ctx, err := e.check(ctx, a)
	if err != nil {
		return exec.Response{}, err
	}
	o.Headers = affinity.StripHeaders(o.Headers)
	if e.ref.Provider() == "claude" {
		o.NativeHistory = exec.NativeHistoryPreserve
		if errPreflight := e.manager.preflightNativeHistory(ctx, e.delegate, a, r, o); errPreflight != nil {
			return exec.Response{}, errPreflight
		}
	}
	return e.delegate.CountTokens(ctx, a, r, o)
}
func (e *affinityExecutor) ExecuteStream(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (*exec.StreamResult, error) {
	ctx, err := e.check(ctx, a)
	if err != nil {
		return nil, err
	}
	o.Headers = affinity.StripHeaders(o.Headers)
	if e.ref.Provider() == "claude" {
		o.NativeHistory = exec.NativeHistoryPreserve
		if errPreflight := e.manager.preflightNativeHistory(ctx, e.delegate, a, r, o); errPreflight != nil {
			return nil, errPreflight
		}
	}
	return e.delegate.ExecuteStream(ctx, a, r, o)
}
func (e *affinityExecutor) Refresh(ctx context.Context, a *Auth) (*Auth, error) {
	return e.delegate.Refresh(ctx, a)
}
func (e *affinityExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	ctx, err := e.check(ctx, a)
	if err != nil {
		return nil, err
	}
	return e.delegate.HttpRequest(ctx, a, r)
}
func (e *affinityExecutor) ShouldPrepareRequestAuth(a *Auth) bool {
	p, ok := e.delegate.(RequestAuthPreparer)
	return ok && p.ShouldPrepareRequestAuth(a)
}
func (e *affinityExecutor) PrepareRequestAuth(ctx context.Context, a *Auth) (*Auth, error) {
	return e.delegate.(RequestAuthPreparer).PrepareRequestAuth(ctx, a)
}
func (e *affinityExecutor) ForAPIKey() ProviderExecutor {
	copy := *e
	if scoped, ok := e.delegate.(APIKeyConfigExecutor); ok {
		copy.delegate = scoped.ForAPIKey()
	}
	return &copy
}

func (m *Manager) observationContextForAuth(ctx context.Context, a *Auth) context.Context {
	ctx = exec.WithQuotaObserver(ctx, nil)
	if m.QuotaAffinityNamespace() == "" || a == nil {
		return ctx
	}
	lease, err := m.quotaLeaseForSnapshot(a)
	if err != nil {
		return ctx
	}
	return m.quotaAttemptContext(ctx, lease)
}

func (e *affinityExecutor) RequestToFormat(r exec.Request, o exec.Options) sdktranslator.Format {
	return requestToFormat(e.ref.Provider(), e.delegate, r, o)
}

func credentialOrigin(a *Auth) string {
	if origin := a.Attributes["base_url"]; origin != "" {
		return origin
	}
	if a.Provider == "claude" {
		return "https://api.anthropic.com"
	}
	return "https://chatgpt.com/backend-api/codex"
}

func checkAffinityCredentialRequest(a *Auth, request *http.Request, usage bool) error {
	if request == nil {
		return nil
	}
	failure := affinity.NewError(affinity.CredentialChanged, time.Time{})
	token := a.Attributes["api_key"]
	if token == "" {
		token, _ = a.Metadata["access_token"].(string)
	}
	authorizationMatches := credentialHeaderMatches(request.Header, "Authorization", "Bearer "+token) && credentialHeaderAbsent(request.Header, "X-Api-Key")
	if a.Provider == "claude" && a.AuthKind() == AuthKindAPIKey && request.URL != nil && request.URL.Scheme == "https" && request.URL.Host == "api.anthropic.com" {
		authorizationMatches = credentialHeaderMatches(request.Header, "X-Api-Key", token) && credentialHeaderAbsent(request.Header, "Authorization")
	}
	if token == "" || !authorizationMatches {
		return failure
	}
	if a.Provider == "codex" && a.AuthKind() == AuthKindOAuth {
		account, _ := a.Metadata["account_id"].(string)
		if account == "" || !credentialHeaderMatches(request.Header, "Chatgpt-Account-Id", account) {
			return failure
		}
	}
	origin := credentialOrigin(a)
	if usage {
		if a.Provider == "claude" {
			origin = "https://api.anthropic.com/api/oauth/usage"
		} else {
			origin = "https://chatgpt.com/backend-api/wham/usage"
		}
	}
	expected, err := url.Parse(origin)
	if err != nil || request.URL == nil {
		return failure
	}
	scheme := request.URL.Scheme
	if scheme == "ws" {
		scheme = "http"
	}
	if scheme == "wss" {
		scheme = "https"
	}
	if scheme != expected.Scheme || request.URL.Host != expected.Host || (request.Host != "" && request.Host != expected.Host) {
		return failure
	}
	path := strings.TrimRight(expected.Path, "/")
	if request.URL.Path != path && !strings.HasPrefix(request.URL.Path, path+"/") {
		return failure
	}
	if usage && (request.URL.Path != expected.Path || request.Method != http.MethodGet || request.URL.RawQuery != "") {
		return failure
	}
	return nil
}

// credentialHeaders inspects the wire map, including intentionally preserved header casing
func credentialHeaders(headers http.Header, name string) []string {
	var values []string
	for key, items := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, items...)
		}
	}
	return values
}
func credentialHeaderMatches(headers http.Header, name, value string) bool {
	values := credentialHeaders(headers, name)
	return len(values) == 1 && values[0] == value
}
func credentialHeaderAbsent(headers http.Header, name string) bool {
	return len(credentialHeaders(headers, name)) == 0
}

// PreflightNativeHistory forwards only the immutable native policy to the delegate
func (e *affinityExecutor) PreflightNativeHistory(ctx context.Context, r exec.Request, o exec.Options, credential exec.NativeHistoryCredential) error {
	preserver, ok := e.delegate.(exec.NativeHistoryPreserver)
	if !ok {
		return affinity.NewError(affinity.PrefixTransformUnsupported, time.Time{})
	}
	o.NativeHistory = exec.NativeHistoryPreserve
	return preserver.PreflightNativeHistory(ctx, r, o, credential)
}
