package console

import "github.com/gopact-ai/steve/internal/consoleapi"

// SealIdle prevents a new exchange racing the restart's idle check.
func (s *Service) SealIdle() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.maintenance {
		return nil, consoleapi.ErrConsoleClosing
	}
	for _, list := range s.exchanges {
		for _, e := range list {
			if e.State == "running" || e.State == "queued" {
				return nil, consoleapi.ErrConsoleClosing
			}
		}
	}
	s.maintenance = true
	return func() { s.mu.Lock(); defer s.mu.Unlock(); s.maintenance = false }, nil
}
