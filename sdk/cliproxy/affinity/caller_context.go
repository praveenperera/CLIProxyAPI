package affinity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type authenticatedCallerKey struct{}

// AuthenticatedCaller isolates a trusted access-provider principal without retaining its secret
func AuthenticatedCaller(provider, principal string) (CallerScope, error) {
	if provider == "" || principal == "" {
		return CallerScope{}, NewError(IdentityRequired, time.Time{})
	}
	digest := sha256.Sum256([]byte(provider + "\x00" + principal))
	return NewCallerScope("caller/v1/" + hex.EncodeToString(digest[:]))
}

// WithAuthenticatedCaller records caller scope at the authenticated host boundary
func WithAuthenticatedCaller(ctx context.Context, caller CallerScope) context.Context {
	return context.WithValue(ctx, authenticatedCallerKey{}, caller)
}

// CallerFromContext returns only a scope captured by the trusted host
func CallerFromContext(ctx context.Context) CallerScope {
	if ctx == nil {
		return CallerScope{}
	}
	caller, _ := ctx.Value(authenticatedCallerKey{}).(CallerScope)
	return caller
}
