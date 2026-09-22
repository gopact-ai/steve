package task

import (
	"errors"
	"github.com/gopact-ai/steve/internal/channel"
)

// TurnInput contains adapter metadata committed with turn admission. It is
// not another persisted address: Task remains the destination's authority.
type TurnInput struct {
	ResumeAdmission ResumeAdmission
	TurnID          string
	Address         channel.Address
	ChatID          string
	ChatType        string
	CardID          string
	Continuation    bool
}

// BeginTurn makes the reply destination and budget charge one durable fact.
// Continuations must keep the original running parent and conversation.
func (s *Store) BeginTurn(id, member, node string, input TurnInput) (Task, error) {
	conversation := ""
	if input.Continuation {
		conversation = input.Address.Conversation
		if conversation == "" {
			return Task{}, errors.New("continuation requires a conversation")
		}
	}
	return s.begin(id, member, node, "", conversation, &input)
}
