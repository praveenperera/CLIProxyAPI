package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
)

// NativeHistoryCredential carries credential configuration for local preflight
// It is not an execution authority or a substitute for the final credential fence
type NativeHistoryCredential struct {
	// Attributes carries executor-facing credential configuration
	Attributes map[string]string
	// Metadata carries stored credential configuration and must not be logged
	Metadata map[string]any
}

// NativeHistoryPreserver validates the full request and configured native policy
// Implementations must perform no network work and must repeat validation at execution
type NativeHistoryPreserver interface {
	PreflightNativeHistory(context.Context, Request, Options, NativeHistoryCredential) error
}

// RequestHooksAllowed is the shared preservation policy for both request hook boundaries
// Strict affinity and native preservation reject hooks before any hook can mutate input
func (o Options) RequestHooksAllowed() bool {
	return o.Affinity.Intent() == affinity.Legacy && o.NativeHistory == NativeHistoryDefault
}
