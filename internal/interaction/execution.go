package interaction

import (
	"context"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// RequestExecution joins auxiliary and delegated questions to their original
// caller proof. Presentation can be a recovery conversation; provenance always
// names the task's actual source, and never the Console owner by default.
func (s *Service) RequestExecution(ctx context.Context, id, kind string, input any, present func() (any, error)) (any, error) {
	if s == nil || s.Book == nil {
		return present()
	}
	var origin Origin
	err := s.Book.Update(ctx, func(tx *ledger.Tx) error {
		a, err := attempt.GetTx(tx, id)
		if err != nil {
			return err
		}
		tracked, found, err := task.GetTx(tx, a.TaskID)
		if err != nil {
			return err
		}
		if !found {
			return identity.ErrDenied
		}
		original := id
		if _, found, err := durable.Get[identity.ExecutionProof](tx, identity.ExecutionKind, id); err != nil {
			return err
		} else if !found {
			original = tracked.AuthorizationAttempt
			var caller *identity.ExecutionProof
			if original == "" {
				pinned, found, err := durable.Get[identity.TaskAuthority](tx, identity.TaskAuthorityKind, tracked.ID)
				if err != nil {
					return err
				}
				if !found || a.Execution == nil || pinned.Epoch != a.Execution.Epoch {
					return identity.ErrDenied
				}
				original = pinned.Attempt
				caller = pinned.Caller
			}
			var proof identity.ExecutionProof
			var err error
			if caller == nil {
				proof, err = callerProof(tx, original)
			} else {
				proof = *caller
				current, e := identity.ResolveAccount(tx, proof.Account)
				err = e
				if e == nil && (current.Principal != proof.Principal || current.Revision != proof.Revision) {
					err = identity.ErrDenied
				}
			}
			if err != nil {
				return err
			}
			if tracked.Transport != proof.Account.Channel {
				return identity.ErrDenied
			}
			if tracked.AuthorizationAttempt == "" && tracked.Requester != proof.Account.ID {
				return identity.ErrDenied
			}
			// This new auxiliary proof inherits the frozen revision, never fresh
			// authority obtained by looking up a current owner or a native ID.
			proof.Attempt = id
			if err := tx.PutBinding(identity.ExecutionKind, id, proof); err != nil {
				return err
			}
		}
		proof, err := identity.CheckExecutionTx(tx, id)
		if err != nil {
			return err
		}
		origin = Origin{Work: durable.Key("execution", id), Address: channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: a.TurnID}, Actor: proof.Account, Task: a.TaskID, Attempt: id}
		return checkOrigin(tx, origin)
	})
	if err != nil {
		return nil, err
	}
	return s.Request(ctx, origin, kind, input, present)
}

func callerProof(tx ledger.Reader, id string) (identity.ExecutionProof, error) {
	for depth := 0; depth < 16; depth++ {
		if _, found, err := durable.Get[identity.ExecutionProof](tx, identity.ExecutionKind, id); err != nil {
			return identity.ExecutionProof{}, err
		} else if found {
			return identity.CheckExecutionTx(tx, id)
		}
		a, err := attempt.GetTx(tx, id)
		if err != nil {
			return identity.ExecutionProof{}, err
		}
		parent, found, err := task.GetTx(tx, a.TaskID)
		if err != nil {
			return identity.ExecutionProof{}, err
		}
		if !found || parent.AuthorizationAttempt == "" {
			return identity.ExecutionProof{}, identity.ErrDenied
		}
		id = parent.AuthorizationAttempt
	}
	return identity.ExecutionProof{}, identity.ErrDenied
}
