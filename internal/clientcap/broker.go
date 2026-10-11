// Package clientcap aggregates authenticated client capabilities. Registration
// advertises availability, not authority. Explicit owner grants and an optional
// invocation approval decide which conversation may use each capability.
package clientcap

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
)

const ClientKind = "client-capability-registration"
const CallKind = "client-capability-invocation"
const ActiveKind = "client-capability-active"
const AttentionKind = "client-capability-attention"

var ErrOffline = errors.New("client is offline")
var ErrCapabilityRemoved = errors.New("client no longer advertises this capability")

func ReadyKind(id string) string { return "client-capability-ready:" + id }

type Capability struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type Registration struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Generation   uint64       `json:"generation"`
	Capabilities []Capability `json:"capabilities"`
	Until        time.Time    `json:"until"`
}
type Register struct {
	Capabilities []Capability `json:"capabilities"`
	Generation   uint64       `json:"generation"`
}
type Request struct {
	Key          string          `json:"key"`
	Client       string          `json:"client"`
	Capability   string          `json:"capability"`
	Conversation string          `json:"conversation"`
	Arguments    json.RawMessage `json:"arguments"`
}

// Execution is fixed by the ACP tool bridge, never supplied by an HTTP caller.
type Execution struct {
	Task         string `json:"task"`
	Attempt      string `json:"attempt"`
	Epoch        uint64 `json:"epoch"`
	Generation   uint64 `json:"generation"`
	LocalRuntime string `json:"local_runtime,omitempty"`
}
type Invocation struct {
	ID               string          `json:"id"`
	Request          Request         `json:"request"`
	Source           string          `json:"source"`
	GrantID          string          `json:"grant_id,omitempty"`
	Execution        *Execution      `json:"execution,omitempty"`
	Depth            int             `json:"depth"`
	State            string          `json:"state"` // approval, pending, claimed, done, failed, unknown, cancelled
	Approved         bool            `json:"approved"`
	Token            string          `json:"claim_token,omitempty"`
	ClientGeneration uint64          `json:"client_generation,omitempty"`
	Incarnation      uint64          `json:"incarnation,omitempty"`
	Runtime          string          `json:"runtime,omitempty"`
	ClaimUntil       time.Time       `json:"claim_until,omitzero"`
	Result           json.RawMessage `json:"result,omitempty"`
	Receipt          string          `json:"receipt,omitempty"`
	Error            string          `json:"error,omitempty"`
	At               time.Time       `json:"at"`
}
type Completion struct {
	Token   string          `json:"claim_token"`
	State   string          `json:"state"`
	Receipt string          `json:"receipt,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}
type Broker struct {
	Book     *ledger.Ledger
	Identity *identity.Service
	Runtime  string
	// CheckExecution checks the original task/attempt/generation and its lease
	// inside request creation and client dispatch. It cannot adopt a newer turn.
	CheckExecution func(*ledger.Tx, Invocation) error
	ExecutionDepth func(ledger.Reader, *Execution) (int, error)
}

func (b *Broker) Register(ctx context.Context, p identity.Principal, input Register) (out Registration, err error) {
	if len(input.Capabilities) > 32 {
		return out, errors.New("a client can register at most 32 capabilities")
	}
	seen := map[string]bool{}
	for _, cap := range input.Capabilities {
		if !identity.ValidName(cap.Name) || seen[cap.Name] || len(cap.Description) > 2048 || len(cap.InputSchema) > 16<<10 || !json.Valid(cap.InputSchema) {
			return out, errors.New("invalid or duplicate capability")
		}
		seen[cap.Name] = true
		var schema map[string]any
		if json.Unmarshal(cap.InputSchema, &schema) != nil || schema["type"] != "object" {
			return out, errors.New("capability input schema must describe an object")
		}
	}
	err = b.Book.Update(ctx, func(tx *ledger.Tx) error {
		grant, err := b.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		if grant.Kind != "client" {
			return identity.ErrDenied
		}
		for name := range seen {
			allowed := false
			for _, cap := range grant.Capabilities {
				if cap.Client == grant.ID && cap.Capability == name {
					allowed = true
				}
			}
			if !allowed {
				return identity.ErrDenied
			}
		}
		old, found, err := durable.Get[Registration](tx, ClientKind, grant.ID)
		if err != nil {
			return err
		}
		if found && old.Generation == input.Generation+1 && reflect.DeepEqual(old.Capabilities, input.Capabilities) {
			out = old
			return nil
		}
		if found && old.Generation != input.Generation || !found && input.Generation != 0 {
			return ledger.ErrConflict
		}
		out = Registration{ID: grant.ID, Name: grant.Name, Generation: input.Generation + 1, Capabilities: input.Capabilities, Until: time.Now().UTC().Add(45 * time.Second)}
		return tx.PutBinding(ClientKind, out.ID, out)
	})
	return
}

func (b *Broker) Heartbeat(ctx context.Context, p identity.Principal, generation uint64) error {
	return b.Book.Update(ctx, func(tx *ledger.Tx) error {
		g, err := b.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		if g.Kind != "client" {
			return identity.ErrDenied
		}
		c, found, err := durable.Get[Registration](tx, ClientKind, g.ID)
		if err != nil {
			return err
		}
		if !found || c.Generation != generation {
			return ledger.ErrStale
		}
		c.Until = time.Now().UTC().Add(45 * time.Second)
		return tx.PutBinding(ClientKind, c.ID, c)
	})
}

func (b *Broker) capability(r ledger.Reader, input Request) (Registration, identity.CapabilityGrant, error) {
	grant, found, err := durable.Get[identity.Grant](r, identity.GrantKind, input.Client)
	if err != nil {
		return Registration{}, identity.CapabilityGrant{}, err
	}
	if !found || !grant.Enabled || grant.Kind != "client" {
		return Registration{}, identity.CapabilityGrant{}, identity.ErrDenied
	}
	var permission identity.CapabilityGrant
	allowed := false
	for _, cap := range grant.Capabilities {
		if cap.Client == input.Client && cap.Capability == input.Capability && cap.Conversation == input.Conversation {
			permission = cap
			allowed = true
			break
		}
	}
	if !allowed {
		return Registration{}, permission, identity.ErrDenied
	}
	client, found, err := durable.Get[Registration](r, ClientKind, input.Client)
	if err != nil {
		return client, permission, err
	}
	if !found || !client.Until.After(time.Now()) {
		return client, permission, ErrOffline
	}
	for _, cap := range client.Capabilities {
		if cap.Name == input.Capability {
			return client, permission, nil
		}
	}
	return client, permission, ErrCapabilityRemoved
}

func (b *Broker) Submit(ctx context.Context, p identity.Principal, input Request, execution *Execution, guard func(*ledger.Tx) error) (out Invocation, err error) {
	if input.Key == "" || len(input.Key) > 512 || len(input.Arguments) > 256<<10 || !json.Valid(input.Arguments) {
		return out, errors.New("client invocation requires a stable key and bounded JSON arguments")
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(input.Arguments, &args) != nil || args == nil {
		return out, errors.New("client capability arguments must be a JSON object")
	}
	err = b.Book.Update(ctx, func(tx *ledger.Tx) error {
		grant, err := b.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		if execution != nil && !p.Owner {
			return identity.ErrDenied
		}
		if !p.Owner {
			allowed := false
			for _, cap := range grant.Capabilities {
				if cap.Client == input.Client && cap.Capability == input.Capability && cap.Conversation == input.Conversation {
					allowed = true
				}
			}
			if !allowed {
				return identity.ErrDenied
			}
		}
		if guard != nil {
			if err := guard(tx); err != nil {
				return err
			}
		}
		source := grant.Principal
		if execution != nil {
			source = "execution:" + execution.Task
		}
		id := durable.Key(source, input.Key)
		out = Invocation{ID: id, Request: input, Source: source, GrantID: p.GrantID, Execution: execution, At: time.Now().UTC(), State: StatePending}
		out.Depth = 1
		if execution != nil && b.ExecutionDepth != nil {
			out.Depth, err = b.ExecutionDepth(tx, execution)
			if err != nil {
				return err
			}
		}
		if b.CheckExecution != nil && execution != nil {
			if err := b.CheckExecution(tx, out); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		_, replay, err := ingress.AcceptTx(tx, ingress.Input{Kind: "client-capability", Source: source, Key: input.Key, Actor: source, Conversation: input.Conversation, Payload: raw}, id, out.At)
		if err != nil {
			return err
		}
		if replay {
			var found bool
			out, found, err = durable.Get[Invocation](tx, CallKind, id)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("client receipt missing invocation")
			}
			return nil
		}
		_, permission, err := b.capability(tx, input)
		if err != nil {
			return err
		}
		if permission.Approval {
			out.State = StateApproval
		} else {
			out.Approved = true
		}
		if err := saveCall(tx, out); err != nil {
			return err
		}
		if out.State == StateApproval {
			raw, _ := json.Marshal(struct{ Invocation, Client, Capability, Conversation string }{id, input.Client, input.Capability, input.Conversation})
			_, err = events.AppendTx(tx, events.Event{Source: "server:client", Key: id, Type: "client.approval", Attention: false, Subject: input.Conversation, Payload: raw, Depth: out.Depth, At: out.At})
			return err
		}
		return nil
	})
	return
}

func saveCall(tx *ledger.Tx, call Invocation) error {
	if err := tx.PutBinding(CallKind, call.ID, call); err != nil {
		return err
	}
	if err := durable.Delete(tx, ReadyKind(call.Request.Client), call.ID); err != nil {
		return err
	}
	if err := durable.Delete(tx, ActiveKind, call.ID); err != nil {
		return err
	}
	if err := durable.Delete(tx, AttentionKind, call.ID); err != nil {
		return err
	}
	if call.State == StateApproval || call.State == StateUnknown {
		return tx.PutBinding(AttentionKind, call.ID, call.ID)
	}
	if call.State == StatePending {
		return tx.PutBinding(ReadyKind(call.Request.Client), call.ID, call.ID)
	}
	if call.State == StateClaimed {
		return tx.PutBinding(ActiveKind, call.ID, call.ID)
	}
	return nil
}

func (b *Broker) Approve(ctx context.Context, id string, allow bool) error {
	return b.Book.Update(ctx, func(tx *ledger.Tx) error {
		call, found, err := durable.Get[Invocation](tx, CallKind, id)
		if err != nil {
			return err
		}
		if !found || call.State != StateApproval {
			return ledger.ErrConflict
		}
		if allow {
			call.State = StatePending
			call.Approved = true
		} else {
			call.State = StateCancelled
		}
		return saveCall(tx, call)
	})
}

func (b *Broker) Poll(ctx context.Context, p identity.Principal, generation uint64) (out Invocation, found bool, err error) {
	var grant identity.Grant
	err = b.Book.Read(ctx, func(tx *ledger.ReadTx) error { var e error; grant, e = b.Identity.Check(tx, p); return e })
	if err != nil {
		return
	}
	if grant.Kind != "client" {
		return out, false, identity.ErrDenied
	}
	ids, _, err := durable.List[string](ctx, b.Book, ReadyKind(grant.ID), "", 32)
	if err != nil {
		return out, false, err
	}
	for _, id := range ids {
		err = b.Book.Update(ctx, func(tx *ledger.Tx) error {
			if _, err := b.Identity.Check(tx, p); err != nil {
				return err
			}
			call, ok, err := durable.Get[Invocation](tx, CallKind, id)
			if err != nil {
				return err
			}
			if !ok || call.State != StatePending {
				return ledger.ErrConflict
			}
			client, _, err := b.capability(tx, call.Request)
			if errors.Is(err, ErrCapabilityRemoved) || errors.Is(err, identity.ErrDenied) {
				call.State = StateCancelled
				call.Error = err.Error()
				return saveCall(tx, call)
			}
			if err != nil {
				return err
			}
			if client.Generation != generation {
				return ledger.ErrStale
			}
			if call.GrantID != "" {
				g, ok, err := durable.Get[identity.Grant](tx, identity.GrantKind, call.GrantID)
				if err != nil {
					return err
				}
				if !ok || !g.Enabled {
					call.State = StateCancelled
					return saveCall(tx, call)
				}
			}
			if b.CheckExecution != nil && call.Execution != nil {
				if err := b.CheckExecution(tx, call); err != nil {
					call.State = StateCancelled
					call.Error = err.Error()
					return saveCall(tx, call)
				}
			}
			call.State = StateClaimed
			call.Token = identity.RandomID()
			call.ClientGeneration = generation
			call.Incarnation = b.Book.Incarnation()
			call.Runtime = b.Runtime
			call.ClaimUntil = time.Now().UTC().Add(time.Minute)
			if err := saveCall(tx, call); err != nil {
				return err
			}
			out = call
			found = true
			return nil
		})
		if errors.Is(err, ledger.ErrConflict) {
			continue
		}
		if err != nil || found {
			return
		}
	}
	return out, false, nil
}

func (b *Broker) Complete(ctx context.Context, p identity.Principal, id string, result Completion) error {
	if result.State != StateDone && result.State != StateFailed && result.State != StateUnknown || result.State == StateDone && result.Receipt == "" || len(result.Receipt) > 4096 || len(result.Error) > 4096 || len(result.Result) > 256<<10 || len(result.Result) > 0 && !json.Valid(result.Result) {
		return errors.New("invalid client completion")
	}
	return b.Book.Update(ctx, func(tx *ledger.Tx) error {
		grant, err := b.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		call, found, err := durable.Get[Invocation](tx, CallKind, id)
		if err != nil {
			return err
		}
		if !found || grant.Kind != "client" || call.Request.Client != grant.ID {
			return identity.ErrDenied
		}
		if result.Token == "" || call.Token != result.Token || call.Incarnation != b.Book.Incarnation() {
			return ledger.ErrStale
		}
		if call.State == StateDone || call.State == StateFailed {
			old, _ := json.Marshal(Completion{Token: call.Token, State: call.State, Receipt: call.Receipt, Result: call.Result, Error: call.Error})
			new, _ := json.Marshal(result)
			if string(old) == string(new) {
				return nil
			}
			return ledger.ErrConflict
		}
		if call.State != StateClaimed && call.State != StateUnknown {
			return ledger.ErrConflict
		}
		call.State = result.State
		call.Result = result.Result
		call.Receipt = result.Receipt
		call.Error = result.Error
		if err := saveCall(tx, call); err != nil {
			return err
		}
		raw, _ := json.Marshal(struct{ Invocation, Client, Capability, State string }{id, call.Request.Client, call.Request.Capability, call.State})
		_, err = events.AppendTx(tx, events.Event{Source: "server:client", Key: id + ":" + call.State, Type: "client.completed", Attention: call.State == StateFailed, Subject: call.Request.Conversation, Payload: raw, Depth: call.Depth, At: time.Now().UTC()})
		return err
	})
}

func (b *Broker) Recover(ctx context.Context, id string) error {
	return b.Book.Update(ctx, func(tx *ledger.Tx) error {
		call, found, err := durable.Get[Invocation](tx, CallKind, id)
		if err != nil || !found {
			return err
		}
		if call.State != StateClaimed || call.Runtime == b.Runtime && call.Incarnation == b.Book.Incarnation() && call.ClaimUntil.After(time.Now()) {
			return nil
		}
		call.State = StateUnknown
		call.Error = "Client action has no receipt; reconcile this invocation instead of replaying the action"
		if err := saveCall(tx, call); err != nil {
			return err
		}
		raw, _ := json.Marshal(struct{ Invocation, Client, Capability string }{id, call.Request.Client, call.Request.Capability})
		_, err = events.AppendTx(tx, events.Event{Source: "server:client", Key: id + ":unknown", Type: "client.unknown", Attention: false, Subject: call.Request.Conversation, Payload: raw, Depth: call.Depth, At: time.Now().UTC()})
		return err
	})
}
