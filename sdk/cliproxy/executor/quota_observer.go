package executor

import (
	"context"
	"net/http"
	"time"
)

type quotaObserverKey struct{}

// QuotaObserver receives provider data at its original header or event boundary
// It remains outside request metadata and must not expose credentials or caller identity
type QuotaObserver func(http.Header, []byte, time.Time)

// WithQuotaObserver attaches a manager-owned observer to one upstream attempt
func WithQuotaObserver(ctx context.Context, observer QuotaObserver) context.Context {
	return context.WithValue(ctx, quotaObserverKey{}, observer)
}

// ObserveQuotaHeaders captures receipt before body reading or stream completion
func ObserveQuotaHeaders(ctx context.Context, headers http.Header) {
	if ctx == nil {
		return
	}
	if observer, ok := ctx.Value(quotaObserverKey{}).(QuotaObserver); ok && observer != nil {
		observer(headers, nil, time.Now().UTC())
	}
}

// ObserveQuotaEvent captures one original provider event at receipt
func ObserveQuotaEvent(ctx context.Context, payload []byte) {
	if ctx == nil {
		return
	}
	if observer, ok := ctx.Value(quotaObserverKey{}).(QuotaObserver); ok && observer != nil {
		observer(nil, payload, time.Now().UTC())
	}
}

// TimedEvent carries original transport receipt through buffering and backpressure
// The transport owner must set ReceivedAt at the read boundary
type TimedEvent struct {
	Payload    []byte
	ReceivedAt time.Time
}

// ObserveQuotaTimedEvent preserves receipt time instead of renewing a queued observation
func ObserveQuotaTimedEvent(ctx context.Context, event TimedEvent) {
	if ctx == nil || event.ReceivedAt.IsZero() {
		return
	}
	if observer, ok := ctx.Value(quotaObserverKey{}).(QuotaObserver); ok && observer != nil {
		observer(nil, event.Payload, event.ReceivedAt)
	}
}
