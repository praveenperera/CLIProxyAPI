package affinity

import (
	"context"
	"time"
)

// BindingStore is independent of token backends and must commit before dispatch
// Implementations must fail closed on corruption and must not evict live bindings
type BindingStore interface {
	// Lookup reads a complete record without extending retention
	Lookup(context.Context, ChatKey) (Binding, bool, error)
	// CreateIfAbsent durably commits one winner before returning
	CreateIfAbsent(context.Context, Binding) (Binding, bool, error)
	// Touch verifies the handle and preserves the credential assignment
	Touch(context.Context, ChatKey, Handle, time.Time) (Binding, error)
	// Prune removes expired records only
	Prune(context.Context, time.Time) error
	// Close releases the lifetime store authority
	Close() error
}

// Retention bounds local state without silently recreating a continuation
type Retention struct {
	idle, hard time.Duration
	capacity   int
}

// NewRetention validates durations and a bounded live capacity
func NewRetention(idle, hard time.Duration, capacity int) (Retention, error) {
	if idle <= 0 || hard < idle || hard > 365*24*time.Hour || capacity < 1 || capacity > 65536 {
		return Retention{}, NewError(IdentityInvalid, time.Time{})
	}
	return Retention{idle, hard, capacity}, nil
}

// DefaultRetention returns seven-day idle and thirty-day hard retention
func DefaultRetention() Retention { return Retention{7 * 24 * time.Hour, 30 * 24 * time.Hour, 65536} }

// Expiration computes expiration from supplied instants, never local quota refill
func (r Retention) Expiration(created, touched time.Time) time.Time {
	return earlier(created.Add(r.hard), touched.Add(r.idle))
}
func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
