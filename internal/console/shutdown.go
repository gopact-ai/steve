package console

import "context"

// Shutdown stops admission and queue draining, cancels running work, and waits
// for the complete runExchange/finish wrapper, including its durable receipt.
// Queued entries stay durable and can be drained by the next service instance.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		s.drained = make(chan struct{})
		for _, list := range s.exchanges {
			for _, e := range list {
				if e.State == "running" && e.cancel != nil {
					e.cancel()
				}
			}
		}
		go func() { s.workers.Wait(); close(s.drained) }()
	}
	drained := s.drained
	s.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
