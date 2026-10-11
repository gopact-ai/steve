package control

import (
	"github.com/gopact-ai/steve/internal/attempt"
	messagechannel "github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/work"
)

// ExecutionCallerTx resolves the immutable caller of this exact execution.
// Child attempts inherit only their recorded parent authorization; conversation
// prefixes, the latest attempt and remembered display mode supply no authority.
func (s *Service) ExecutionCallerTx(tx ledger.Reader, id string) (identity.ExecutionProof, task.Task, error) {
	r, err := attempt.GetTx(tx, id)
	if err != nil {
		return identity.ExecutionProof{}, task.Task{}, err
	}
	if r.State != attempt.Running || r.Execution == nil || r.SessionSettled == nil || *r.SessionSettled || r.Unsettled || r.SupersededBy != "" {
		return identity.ExecutionProof{}, task.Task{}, ErrDenied
	}
	if err := task.CheckExecutionTx(tx, r.Execution); err != nil {
		return identity.ExecutionProof{}, task.Task{}, err
	}
	tracked, found, err := task.GetTx(tx, r.TaskID)
	if err != nil || !found {
		return identity.ExecutionProof{}, task.Task{}, ErrDenied
	}
	for depth := 0; tracked.AuthorizationAttempt != ""; depth++ {
		if depth >= 16 {
			return identity.ExecutionProof{}, task.Task{}, ErrDenied
		}
		r, err = attempt.GetTx(tx, tracked.AuthorizationAttempt)
		if err != nil {
			return identity.ExecutionProof{}, task.Task{}, err
		}
		if r.TaskID != tracked.Parent {
			return identity.ExecutionProof{}, task.Task{}, ErrDenied
		}
		tracked, found, err = task.GetTx(tx, r.TaskID)
		if err != nil || !found {
			return identity.ExecutionProof{}, task.Task{}, ErrDenied
		}
	}
	proof, err := identity.CheckExecutionTx(tx, r.ID)
	if err != nil {
		return proof, tracked, err
	}
	if proof.Account.Channel != tracked.Transport || proof.Account.ID != r.By {
		return proof, tracked, ErrDenied
	}
	// BeginTurn updates the task's current chat type. Only the exact accepted
	// input proves whether this original caller asked in private.
	tracked.ChatType = ""
	address := messagechannel.Address{Channel: proof.Account.Channel, Conversation: tracked.Channel, Message: r.TurnID}
	input, found, err := durable.Get[work.InputRecord](tx, work.InputKind, durable.Key(address.Channel, address.Conversation, address.Message))
	if err != nil {
		return proof, tracked, err
	}
	if found {
		if input.Address != address || input.Actor != proof.Account || input.Principal != proof.Principal || input.AccountRevision != proof.Revision {
			return proof, tracked, ErrDenied
		}
		tracked.ChatType = string(input.ChatType)
	}
	return proof, tracked, nil
}
func (s *Service) ExecutionOwnerTx(tx ledger.Reader, id string, delegated bool) error {
	proof, tracked, err := s.ExecutionCallerTx(tx, id)
	if err != nil {
		return err
	}
	if s.book() == nil || proof.Principal != s.Owners.Configuration().Accounts.Owner || tracked.ChatType != string(protocol.ChatP2P) || delegated {
		return ErrDenied
	}
	return nil
}
