package usage

import "context"

// spawnDispatcher runs the dispatcher in a goroutine and records a channel that closes once it
// has drained the queue, so Wait can block on delivery after Stop
func (m *Manager) spawnDispatcher(workerCtx context.Context) {
	m.mu.Lock()
	m.done = make(chan struct{})
	done := m.done
	m.mu.Unlock()
	go func() {
		defer close(done)
		m.run(workerCtx)
	}()
}

// Wait blocks until the dispatcher has delivered every queued record after Stop, or ctx ends
func (m *Manager) Wait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	if done == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitDefault waits for the default manager to deliver queued records after StopDefault
func WaitDefault(ctx context.Context) error { return DefaultManager().Wait(ctx) }
