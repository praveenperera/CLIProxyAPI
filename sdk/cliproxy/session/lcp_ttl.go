package session

import "time"

// SetTTL changes how long later matches and refreshes last; existing expiries are kept
func (m *MerklePrefixMatcher) SetTTL(ttl time.Duration) {
	if m == nil || ttl <= 0 {
		return
	}
	m.mu.Lock()
	m.ttl = ttl
	m.mu.Unlock()
}
