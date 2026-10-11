// Package work is the controlled intelligence entrance for providers, clients
// and automation. Its Conversation port durably admits work to the existing
// serialized queue; it never calls a harness directly or creates a replacement
// attempt after uncertain execution.
package work

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
)

const RecordKind = "service-work"
const ReadyKind = "service-work-ready"

var ErrAdmission = errors.New("work admission refused")

type Request struct {
	Key          string          `json:"key"`
	Conversation string          `json:"conversation"`
	Project      string          `json:"project"`
	Instruction  string          `json:"instruction"`
	Context      json.RawMessage `json:"context,omitempty"`
	Causation    string          `json:"causation,omitempty"`
}

type Record struct {
	ID       string    `json:"id"`
	GrantID  string    `json:"grant_id,omitempty"`
	Owner    bool      `json:"owner"`
	Request  Request   `json:"request"`
	State    string    `json:"state"`
	Exchange string    `json:"exchange,omitempty"`
	Outcome  *Result   `json:"outcome,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	At       time.Time `json:"at"`
}

type Result struct {
	Exchange string `json:"exchange"`
	State    string `json:"state"`
	Task     string `json:"task,omitempty"`
	Attempt  string `json:"attempt,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Conversation interface {
	AcceptWork(context.Context, Record, func(*ledger.Tx, string) error) (string, error)
	WorkResult(context.Context, string, string) (Result, bool, error)
	ValidateWork(context.Context, string, string, string) error
}

type Service struct {
	Book            *ledger.Ledger
	Identity        *identity.Service
	Conversations   Conversation
	AuthorizeRecord func(*ledger.Tx, Record) error
}

func (s *Service) Submit(ctx context.Context, p identity.Principal, input Request) (out Record, err error) {
	if input.Key == "" || len(input.Key) > 512 || (input.Conversation == "" || len(input.Conversation) > 512) || input.Project == "" || strings.TrimSpace(input.Instruction) == "" || len(input.Instruction) > 16<<10 || len(input.Context) > 256<<10 || len(input.Context) > 0 && !json.Valid(input.Context) {
		return out, errors.New("work: stable key, conversation, project and bounded instruction/context are required")
	}
	var existing bool
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		g, e := s.Identity.Check(tx, p)
		if e != nil {
			return e
		}
		if !g.AllowsWork(input.Conversation, input.Project) {
			return identity.ErrDenied
		}
		_, existing, e = durable.Get[Record](tx, RecordKind, durable.Key(g.Principal, input.Key))
		return e
	})
	if err != nil {
		return
	}
	if !existing {
		if err = s.Conversations.ValidateWork(ctx, input.Conversation, input.Project, input.Instruction); err != nil {
			return
		}
	}
	raw, e := json.Marshal(input)
	if e != nil {
		return out, e
	}
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		grant, err := s.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		if !grant.AllowsWork(input.Conversation, input.Project) {
			return identity.ErrDenied
		}
		id := durable.Key(grant.Principal, input.Key)
		_, replay, err := ingress.AcceptTx(tx, ingress.Input{Kind: "work", Source: grant.Principal, Key: input.Key, Actor: grant.Principal, Conversation: input.Conversation, Payload: raw}, id, time.Now().UTC())
		if err != nil {
			return err
		}
		if replay {
			var found bool
			out, found, err = durable.Get[Record](tx, RecordKind, id)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("work receipt missing its work record")
			}
			return nil
		}
		out = Record{ID: id, GrantID: p.GrantID, Owner: p.Owner, Request: input, State: StatePending, At: time.Now().UTC()}
		if err := tx.PutBinding(RecordKind, id, out); err != nil {
			return err
		}
		return tx.PutBinding(ReadyKind, id, id)
	})
	return
}

// Dispatch commits the work receipt in the SAME transaction as queue admission.
// A crash before it leaves ready work; a crash after it replays the original
// exchange. Revocation is checked again in that transaction.
func (s *Service) Dispatch(ctx context.Context, id string) error {
	record, found, err := durable.Read[Record](ctx, s.Book, RecordKind, id)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("work not found")
	}
	if record.State != StatePending {
		return nil
	}
	_, err = s.Conversations.AcceptWork(ctx, record, func(tx *ledger.Tx, exchange string) error {
		current, found, err := durable.Get[Record](tx, RecordKind, id)
		if err != nil {
			return err
		}
		if !found || current.State != StatePending {
			return ledger.ErrConflict
		}
		if s.AuthorizeRecord != nil {
			if err := s.AuthorizeRecord(tx, current); err != nil {
				return err
			}
		}
		grant, err := s.Identity.Check(tx, identity.Principal{GrantID: record.GrantID, Owner: record.Owner})
		if err != nil {
			return err
		}
		if !grant.AllowsWork(record.Request.Conversation, record.Request.Project) {
			return identity.ErrDenied
		}
		current.State = StateAccepted
		current.Exchange = exchange
		if err := tx.PutBinding(RecordKind, id, current); err != nil {
			return err
		}
		return durable.Delete(tx, ReadyKind, id)
	})
	if err != nil && !errors.Is(err, ledger.ErrConflict) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		// Refusals remain visible; transient queue/service shutdown errors are left
		// to the caller to classify. Never start another ACP turn to hide failure.
		if errors.Is(err, identity.ErrDenied) || errors.Is(err, ErrAdmission) {
			return s.fail(ctx, id, err)
		}
	}
	return err
}

func (s *Service) fail(ctx context.Context, id string, cause error) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		record, found, err := durable.Get[Record](tx, RecordKind, id)
		if err != nil || !found {
			return err
		}
		if record.State != StatePending {
			return nil
		}
		record.State = StateFailed
		record.Detail = cause.Error()
		if err := tx.PutBinding(RecordKind, id, record); err != nil {
			return err
		}
		return durable.Delete(tx, ReadyKind, id)
	})
}

func (s *Service) Status(ctx context.Context, p identity.Principal, id string) (record Record, result Result, err error) {
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		grant, e := s.Identity.Check(tx, p)
		if e != nil {
			return e
		}
		var found bool
		record, found, e = durable.Get[Record](tx, RecordKind, id)
		if e != nil {
			return e
		}
		if !found {
			return errors.New("work not found")
		}
		if !p.Owner && (record.GrantID != grant.ID || !grant.AllowsWork(record.Request.Conversation, record.Request.Project)) {
			return identity.ErrDenied
		}
		return nil
	})
	if err != nil || record.Exchange == "" {
		return
	}
	if record.Outcome != nil {
		return record, *record.Outcome, nil
	}
	var found bool
	result, found, err = s.Conversations.WorkResult(ctx, record.Request.Conversation, record.Exchange)
	if err == nil && !found {
		err = errors.New("accepted work has no retained exchange")
	}
	return
}
