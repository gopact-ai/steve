package readmodel

import "github.com/gopact-ai/steve/internal/consoleapi"

func (m *Model) SetInteractions(source interface {
	Questions(string) []consoleapi.PendingQuestion
}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interactions = source
}

func (m *Model) pendingInteractions() []HumanRequest {
	m.mu.Lock()
	source := m.interactions
	m.mu.Unlock()
	if source == nil {
		return nil
	}
	var out []HumanRequest
	for _, question := range source.Questions("") {
		if question.State != "pending" {
			continue
		}
		out = append(out, HumanRequest{ID: question.ID, Type: "question", Source: question.ID, TaskID: question.TaskID, AttemptID: question.AttemptID, ProjectID: question.Project, Conversation: question.Conversation, Summary: question.Message, Choices: []Choice{}, CreatedAt: question.CreatedAt, Resolvable: true})
	}
	return out
}
