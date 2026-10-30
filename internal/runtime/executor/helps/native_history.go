package helps

import (
	"context"
	"net/http"
)

type nativeHistoryPreservationKey struct{}

// WithNativeHistoryPreservation selects honest caller-owned native transport headers
func WithNativeHistoryPreservation(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, nativeHistoryPreservationKey{}, true)
}

// NativeHistoryPreservation reports the explicit executor transport policy
func NativeHistoryPreservation(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	preserve, _ := ctx.Value(nativeHistoryPreservationKey{}).(bool)
	return preserve
}

// NativeFingerprintOverride keeps the caller fingerprint and drops the confirmed Claude Code
// profile under native preservation, and reports whether preservation is active
func NativeFingerprintOverride(ctx context.Context, preserveCallerFingerprint, confirmedClaudeCode *bool) bool {
	if !NativeHistoryPreservation(ctx) {
		return false
	}
	*preserveCallerFingerprint = true
	*confirmedClaudeCode = false
	return true
}

// ApplyNativeBetaHeader sends exactly the caller betas under native preservation and reports
// whether it did, so the caller skips the CLI beta policy
func ApplyNativeBetaHeader(nativePreserve bool, header http.Header, betas string) bool {
	if !nativePreserve {
		return false
	}
	if betas != "" {
		header.Set("Anthropic-Beta", betas)
	} else {
		header.Del("Anthropic-Beta")
	}
	return true
}
