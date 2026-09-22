package console

// ExchangeConversation exposes only ownership, not the queued prompt/history.
// Queue controls must resolve their server-owned identity before acting.
func (s *Service) ExchangeConversation(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conversation, list := range s.exchanges {
		for _, e := range list {
			if e.ID == id {
				return conversation, true
			}
		}
	}
	return "", false
}
