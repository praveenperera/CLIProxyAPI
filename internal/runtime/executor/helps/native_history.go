package helps

import "context"

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
