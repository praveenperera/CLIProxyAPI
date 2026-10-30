package auth

// prepareSelectorSwap readies a replacement selector while m.mu is held, before SetSelector
// publishes it
func (m *Manager) prepareSelectorSwap(selector, oldSelector Selector) {
	// bind before publishing so concurrent Picks never see the quota source change
	m.bindSelectorQuota(selector)
	if next, ok := selector.(*SessionAffinitySelector); ok {
		if previous, okPrevious := oldSelector.(*SessionAffinitySelector); okPrevious {
			next.adoptBindings(previous)
		}
	}
}

// adoptBindings takes over a replaced selector's session bindings before s is published, so a
// routing config change never moves a bound session off its credential and its prompt cache
func (s *SessionAffinitySelector) adoptBindings(previous *SessionAffinitySelector) {
	if s == nil || previous == nil || s == previous || previous.cache == nil || previous.matcher == nil {
		return
	}
	unused := s.cache
	s.cache, s.matcher = previous.cache, previous.matcher
	s.cache.setTTL(s.ttl)
	s.matcher.SetTTL(s.ttl)
	previous.handedOff.Store(true)
	unused.Stop()
}
