package work

import (
	"context"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turnapi"
)

// PinTask fixes the caller of a /plan before any auxiliary execution starts.
// Rule planning has no chat attempt, so its proof originates in accepted Work.
func (s *Engine) PinTask(ctx context.Context, taskID string, req turnapi.Request) error {
	if s == nil || s.Book == nil {
		return nil
	}
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		address := req.Address()
		input, found, err := durable.Get[InputRecord](tx, InputKind, durable.Key(address.Channel, address.Conversation, address.Message))
		if err != nil {
			return err
		}
		if !found {
			return identity.ErrDenied
		}
		tracked, found, err := task.GetTx(tx, taskID)
		if err != nil {
			return err
		}
		if !found || tracked.Requester != input.Actor.ID || tracked.Transport != input.Actor.Channel || tracked.Channel != input.Address.Conversation {
			return identity.ErrDenied
		}
		account, err := identity.ResolveAccount(tx, input.Actor)
		if err != nil {
			return err
		}
		if account.Principal != input.Principal || account.Revision != input.AccountRevision {
			return identity.ErrDenied
		}
		proof := identity.ExecutionProof{Account: input.Actor, Principal: input.Principal, Revision: input.AccountRevision}
		return tx.PutBinding(identity.TaskAuthorityKind, taskID, identity.TaskAuthority{Epoch: tracked.ExecutionEpoch, Caller: &proof})
	})
}

// BindExecutionAccountTx preserves the account snapshot taken at input
// acceptance. Known legacy attempts without a Work receipt use their exact
// recorded native source; a new input cannot adopt a later account binding.
func BindExecutionAccountTx(tx *ledger.Tx, attempt string, address channel.Address, account identity.Account) error {
	record, found, err := durable.Get[InputRecord](tx, InputKind, durable.Key(address.Channel, address.Conversation, address.Message))
	if err != nil {
		return err
	}
	if found {
		current, err := identity.ResolveAccount(tx, account)
		if err != nil {
			return err
		}
		if record.Address != address || record.Actor != account || record.Principal != current.Principal || record.AccountRevision != current.Revision {
			return identity.ErrDenied
		}
	}
	return identity.BindExecutionTx(tx, attempt, account)
}

// completionAttempt follows only the durably authorized succession of the
// original attempt. Work never substitutes an unrelated successful execution.
func completionAttempt(tx ledger.Reader, input InputRecord, result string) (attempt.Record, error) {
	id := input.Attempt
	for depth := 0; depth < 16; depth++ {
		a, err := attempt.GetTx(tx, id)
		if err != nil {
			return attempt.Record{}, err
		}
		if a.TaskID != input.Task || a.TurnID != input.Address.Message {
			return attempt.Record{}, identity.ErrDenied
		}
		if result == id || result == "" && a.SupersededBy == "" {
			return a, nil
		}
		if a.State != attempt.Superseded || a.Unsettled || a.SupersededBy == "" {
			return attempt.Record{}, ledger.ErrConflict
		}
		id = a.SupersededBy
	}
	return attempt.Record{}, ledger.ErrConflict
}

// CheckOutputAuthorityTx rechecks an accepted input before a new external
// delivery. A retained successful command bypasses sending entirely.
func CheckOutputAuthorityTx(tx ledger.Reader, address channel.Address) error {
	input, found, err := durable.Get[InputRecord](tx, InputKind, durable.Key(address.Channel, address.Conversation, address.Message))
	if err != nil || !found {
		return err
	}
	current, err := identity.ResolveAccount(tx, input.Actor)
	if err != nil {
		return err
	}
	if current.Principal != input.Principal || current.Revision != input.AccountRevision {
		return identity.ErrDenied
	}
	owner, configured, err := durable.Get[string](tx, identity.AuthorityKind, "owner")
	if err != nil || !configured || input.Task == "" {
		return err
	}
	tracked, found, err := task.GetTx(tx, input.Task)
	if err != nil {
		return err
	}
	if !found {
		return identity.ErrDenied
	}
	role, err := project.AccessPolicyTx(tx, tracked.ProjectID, input.Principal, owner)
	if err != nil {
		return err
	}
	if !role.AtLeast(project.RoleRead) {
		return identity.ErrDenied
	}
	return nil
}
