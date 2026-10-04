package cliproxy

import "context"

func (s *Service) startQuotaPoller(ctx context.Context) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.quotaStopped || s.quotaPollCancel != nil || s.coreManager == nil || s.coreManager.QuotaAffinityNamespace() == "" {
		return
	}
	pollCtx, cancel := context.WithCancel(ctx)
	s.quotaPollCancel = cancel
	s.quotaPollWG.Add(1)
	go func() { defer s.quotaPollWG.Done(); s.coreManager.RunQuotaPoller(pollCtx) }()
}
func (s *Service) stopQuotaPoller() {
	s.quotaMu.Lock()
	s.quotaStopped = true
	cancel := s.quotaPollCancel
	s.quotaMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.quotaPollWG.Wait()
}
