package executor

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
)

type credentialFenceKey struct{}

// WithCredentialFence installs the manager's immutable authority check for every network dispatch
func WithCredentialFence(ctx context.Context, check func(*http.Request) error) context.Context {
	return context.WithValue(ctx, credentialFenceKey{}, check)
}

// CheckCredentialFence checks the exact local credential before a send or reconnect
// Legacy contexts have no strict fence
func CheckCredentialFence(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if check, ok := ctx.Value(credentialFenceKey{}).(func(*http.Request) error); ok {
		return check(nil)
	}
	return nil
}

type credentialTransport struct{ delegate http.RoundTripper }

func (t credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := CheckCredentialRequest(req.Context(), req); err != nil {
		return nil, err
	}
	request := req.Clone(req.Context())
	request.Header = affinity.StripHeaders(req.Header)
	return t.delegate.RoundTrip(request)
}

// CredentialTransport strips local headers and enforces a manager fence at HTTP dispatch
func CredentialTransport(ctx context.Context, delegate http.RoundTripper) http.RoundTripper {
	if ctx == nil {
		return delegate
	}
	if _, strict := ctx.Value(credentialFenceKey{}).(func(*http.Request) error); !strict {
		return delegate
	}
	if delegate == nil {
		delegate = http.DefaultTransport
	}
	return credentialTransport{delegate}
}

// LocalExecutionAuthority identifies an executor implemented and owned by the local host
// Plugin and Home executors must not implement this authority
type LocalExecutionAuthority interface{ LocalBuiltinProvider() string }

// CheckCredentialRequest validates the final credential headers and destination before transport dispatch
func CheckCredentialRequest(ctx context.Context, request *http.Request) error {
	if ctx == nil {
		return nil
	}
	if check, ok := ctx.Value(credentialFenceKey{}).(func(*http.Request) error); ok {
		return check(request)
	}
	return nil
}
