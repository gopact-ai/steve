// Package interaction owns execution-bound questions and their response facts.
// Native adapters render and authenticate replies; this module fences their
// lifetime and records decisions without granting execution or starting work.
package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"time"
)

const RecordKind = "interaction"
const ActiveKind = "interaction-active"
const RuntimeKind = "work-runtime"

type Origin struct {
	Work    string           `json:"work"`
	Address channel.Address  `json:"address"`
	Actor   identity.Account `json:"actor"`
	Task    string           `json:"task,omitempty"`
	Attempt string           `json:"attempt,omitempty"`
}
type Record struct {
	ID        string          `json:"id"`
	Origin    Origin          `json:"origin"`
	Principal string          `json:"principal"`
	Kind      string          `json:"kind"`
	Input     json.RawMessage `json:"input"`
	Response  json.RawMessage `json:"response,omitempty"`
	State     string          `json:"state"`
	Runtime   string          `json:"runtime"`
	CreatedAt time.Time       `json:"created_at"`
	Deadline  time.Time       `json:"deadline"`
	Detail    string          `json:"detail,omitempty"`
}

type Service struct {
	Book    *ledger.Ledger
	Runtime string
	scan    durable.Scanner
}

// Request records the original scope before exposing a question. A response
// from an ended attempt or a different application activation grants nothing.
func (s *Service) Request(ctx context.Context, origin Origin, kind string, input any, present func() (any, error)) (response any, err error) {
	if s == nil || s.Book == nil {
		return present()
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	// Execution context and the presenter own question timeouts. Native
	// questions may intentionally wait while their node is offline.
	record := Record{ID: identity.RandomID(), Origin: origin, Kind: kind, Input: raw, State: "pending", Runtime: s.Runtime, CreatedAt: now}
	if until, ok := ctx.Deadline(); ok {
		record.Deadline = until
	}
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		active, found, err := durable.Get[string](tx, RuntimeKind, "active")
		if err != nil {
			return err
		}
		if !found || active != s.Runtime {
			return ledger.ErrStale
		}
		binding, err := identity.ResolveAccount(tx, origin.Actor)
		if err != nil {
			return err
		}
		record.Principal = binding.Principal
		if err := checkOrigin(tx, origin); err != nil {
			return err
		}
		if err := tx.PutBinding(RecordKind, record.ID, record); err != nil {
			return err
		}
		return tx.PutBinding(ActiveKind, record.ID, record.ID)
	})
	if err != nil {
		return nil, err
	}
	response, err = present()
	encoded, encodeErr := json.Marshal(response)
	if encodeErr != nil {
		err = errors.Join(err, encodeErr)
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	commitErr := s.Book.Update(finish, func(tx *ledger.Tx) error {
		current, found, e := durable.Get[Record](tx, RecordKind, record.ID)
		if e != nil {
			return e
		}
		if !found || current.Runtime != s.Runtime || current.State != "pending" || !current.Deadline.IsZero() && !current.Deadline.After(time.Now()) {
			return ledger.ErrConflict
		}
		active, found, e := durable.Get[string](tx, RuntimeKind, "active")
		if e != nil {
			return e
		}
		if !found || active != s.Runtime {
			return ledger.ErrStale
		}
		current.State = "answered"
		if err != nil {
			current.State = "interrupted"
			current.Detail = err.Error()
		} else if e := checkOrigin(tx, origin); e != nil {
			current.State = "interrupted"
			current.Detail = e.Error()
			err = e
		}
		binding, e := identity.ResolveAccount(tx, origin.Actor)
		if e != nil || binding.Principal != record.Principal {
			current.State = "interrupted"
			current.Detail = "question authority changed"
			err = identity.ErrDenied
		}
		if current.State == "answered" {
			current.Response = encoded
		}
		if err := tx.PutBinding(RecordKind, current.ID, current); err != nil {
			return err
		}
		return durable.Delete(tx, ActiveKind, current.ID)
	})
	return response, errors.Join(err, commitErr)
}

func checkOrigin(r ledger.Reader, origin Origin) error {
	if origin.Work == "" || origin.Address.Channel == "" || origin.Address.Conversation == "" {
		return identity.ErrDenied
	}
	if origin.Attempt == "" {
		return identity.ErrDenied
	}
	a, err := attempt.GetTx(r, origin.Attempt)
	if err != nil {
		return err
	}
	if a.TaskID != origin.Task || a.State != attempt.Running || a.Unsettled {
		return identity.ErrDenied
	}
	if a.Execution == nil {
		return identity.ErrDenied
	}
	if err := task.CheckExecutionTx(r, a.Execution); err != nil {
		return err
	}
	proof, err := identity.CheckExecutionTx(r, origin.Attempt)
	if err != nil {
		return err
	}
	tracked, found, err := task.GetTx(r, origin.Task)
	if err != nil {
		return err
	}
	if !found || tracked.Transport != origin.Address.Channel || tracked.Channel != origin.Address.Conversation || proof.Account != origin.Actor {
		return identity.ErrDenied
	}
	owner, _, err := durable.Get[string](r, identity.AuthorityKind, "owner")
	if err != nil {
		return err
	}
	role, err := project.AccessPolicyTx(r, a.Project, proof.Principal, owner)
	if err != nil {
		return err
	}
	if !role.AtLeast(project.RoleWrite) {
		return identity.ErrDenied
	}
	return nil
}
func (s *Service) Records(ctx context.Context, after string) (durable.Page[Record], error) {
	return durable.PageOf[Record](ctx, s.Book, RecordKind, after)
}

// Pump expires unanswered questions and retires questions whose original
// presenter belonged to an ended activation. It never synthesizes approval.
func (s *Service) Pump(ctx context.Context) error {
	ids, err := s.scan.Next(ctx, s.Book, ActiveKind, 32)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.Book.Update(ctx, func(tx *ledger.Tx) error {
			r, found, err := durable.Get[Record](tx, RecordKind, id)
			if err != nil {
				return err
			}
			if !found {
				return durable.Delete(tx, ActiveKind, id)
			}
			if r.Runtime == s.Runtime && (r.Deadline.IsZero() || r.Deadline.After(time.Now())) {
				return nil
			}
			r.State = "interrupted"
			r.Detail = "original presentation expired or restarted"
			if err := tx.PutBinding(RecordKind, id, r); err != nil {
				return err
			}
			return durable.Delete(tx, ActiveKind, id)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Run(ctx context.Context) {
	if s == nil {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		_ = s.Pump(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
