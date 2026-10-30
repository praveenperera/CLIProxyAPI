package auth

import (
	"context"
	"time"
)

// NewTransientRetryTestManager builds a manager with a deterministic backoff for cross-package tests
func NewTransientRetryTestManager() *Manager {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.transientRetryWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	m.SetTransientRetryConfig(2)
	return m
}
