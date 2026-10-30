package executor

import (
	"context"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
)

type transportIdentityKey struct{}
type transportIdentity struct {
	credential affinity.CredentialRef
	scope      affinity.ChatKey
}

// WithTransportIdentity binds strict transport reuse to verified credential and caller/chat scope
func WithTransportIdentity(ctx context.Context, credential affinity.CredentialRef, scope affinity.ChatKey) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, transportIdentityKey{}, transportIdentity{credential, scope})
}

// TransportBinding retains the actual handshake authority outside mutable request metadata
// Its private request is never emitted in logs or exposed to callers
type TransportBinding struct {
	identity  transportIdentity
	handshake *http.Request
}

// NewTransportBinding validates and saves an independent handshake credential snapshot
func NewTransportBinding(ctx context.Context, request *http.Request) (TransportBinding, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request == nil {
		return TransportBinding{}, affinity.NewError(affinity.CredentialChanged, time.Time{})
	}
	if err := CheckCredentialRequest(ctx, request); err != nil {
		return TransportBinding{}, err
	}
	identity, _ := ctx.Value(transportIdentityKey{}).(transportIdentity)
	handshake := request.Clone(context.Background())
	handshake.Header = request.Header.Clone()
	return TransportBinding{identity, handshake}, nil
}

// Check validates execution scope, current lineage, and the actual wire credential on reuse or send
func (b TransportBinding) Check(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	identity, _ := ctx.Value(transportIdentityKey{}).(transportIdentity)
	if identity != b.identity {
		return affinity.NewError(affinity.CredentialChanged, time.Time{})
	}
	if b.handshake == nil {
		if identity != (transportIdentity{}) {
			return affinity.NewError(affinity.CredentialChanged, time.Time{})
		}
		return CheckCredentialFence(ctx)
	}
	return CheckCredentialRequest(ctx, b.handshake)
}
