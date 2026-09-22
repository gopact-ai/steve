package gateway

// ConversationBusy reports this process's live input owner, including waking,
// execution and delivery. Persisted unfinished accounting alone cannot prove
// that a disconnected or recovering agent is still executing.
func (g *Gateway) ConversationBusy(conversation string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.serving[conversation] > 0
}
