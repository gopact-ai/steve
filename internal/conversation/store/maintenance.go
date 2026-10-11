package store

import "github.com/gopact-ai/steve/internal/conversationapi"

// SealIdle prevents a new exchange racing the restart's idle check.
func (s *Service) SealIdle() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.maintenance {
		return nil, conversationapi.ErrConsoleClosing
	}
	for _, list := range s.exchanges {
		for _, e := range list {
			if e.State == conversationapi.ExchangeRunning || e.State == conversationapi.ExchangeQueued {
				return nil, conversationapi.ErrConsoleClosing
			}
		}
	}
	s.maintenance = true
	return func() { s.mu.Lock(); defer s.mu.Unlock(); s.maintenance = false }, nil
}
