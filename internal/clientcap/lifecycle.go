package clientcap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func capabilityContract(client Registration, name string) (Capability, string, error) {
	for _, cap := range client.Capabilities {
		if cap.Name != name {
			continue
		}
		schema, err := durable.CanonicalJSON(cap.InputSchema)
		if err != nil {
			return Capability{}, "", err
		}
		cap.InputSchema = schema
		raw, err := json.Marshal(cap)
		return cap, durable.Key(string(raw)), err
	}
	return Capability{}, "", ErrCapabilityRemoved
}

func permits(g identity.Grant, request Request) bool {
	_, allowed := requestPermission(g, request)
	return allowed
}

func requestPermission(g identity.Grant, request Request) (identity.CapabilityGrant, bool) {
	for _, scope := range g.Capabilities {
		if scope.Client == request.Client && scope.Capability == request.Capability && scope.Conversation == request.Conversation {
			return scope, true
		}
	}
	return identity.CapabilityGrant{}, false
}

func (b *Broker) checkUnclaimedTx(tx *ledger.Tx, call Invocation) error {
	g, found, err := durable.Get[identity.Grant](tx, identity.GrantKind, call.Request.Client)
	if err != nil {
		return err
	}
	permission, allowed := requestPermission(g, call.Request)
	if !found || !g.Enabled || g.Kind != "client" || !allowed {
		return identity.ErrDenied
	}
	if call.ClientPermission == nil || *call.ClientPermission != permission {
		return fmt.Errorf("%w: client permission changed", identity.ErrDenied)
	}
	if call.GrantID != "" {
		caller, found, err := durable.Get[identity.Grant](tx, identity.GrantKind, call.GrantID)
		if err != nil {
			return err
		}
		permission, allowed := requestPermission(caller, call.Request)
		if !found || !caller.Enabled || !allowed || call.CallerPermission == nil || *call.CallerPermission != permission {
			return fmt.Errorf("%w: caller permission changed", identity.ErrDenied)
		}
	}
	if b.CheckExecution != nil && call.Execution != nil {
		if err := b.CheckExecution(tx, call); err != nil {
			return err
		}
	}
	client, found, err := durable.Get[Registration](tx, ClientKind, call.Request.Client)
	if err != nil {
		return err
	}
	if !found {
		return ErrCapabilityRemoved
	}
	_, digest, err := capabilityContract(client, call.Request.Capability)
	if err != nil {
		return err
	}
	// A legacy unclaimed call has no historical contract proof. Never adopt the
	// current definition merely because the capability name is still present.
	if call.Contract == nil || call.ContractDigest == "" || call.ContractDigest != digest {
		return ErrContractChanged
	}
	if !client.Until.After(time.Now()) {
		return ErrOffline
	}
	return nil
}

func permanentCallError(err error) bool {
	return errors.Is(err, identity.ErrDenied) || errors.Is(err, ErrCapabilityRemoved) || errors.Is(err, ErrContractChanged) || errors.Is(err, ledger.ErrStale) || errors.Is(err, task.ErrExecutionStopped) || errors.Is(err, attempt.ErrNotFound)
}

// SweepUnclaimed makes authority expiry and consumer retirement independent of
// another client poll. It never scans or reissues dispatched uncertain actions.
func (b *Broker) SweepUnclaimed(ctx context.Context) error {
	ids, err := b.scan.NextStates(ctx, b.Book, CallKind, []string{StateApproval, StatePending}, 32)
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		err := b.Book.Update(ctx, func(tx *ledger.Tx) error {
			call, found, err := durable.Get[Invocation](tx, CallKind, id)
			if err != nil || !found {
				return err
			}
			if call.State != StateApproval && call.State != StatePending {
				return nil
			}
			err = b.checkUnclaimedTx(tx, call)
			if permanentCallError(err) {
				call.State = StateCancelled
				call.Error = err.Error()
				return saveCall(tx, call)
			}
			if err != nil && !errors.Is(err, ErrOffline) {
				return err
			}
			wait := b.WaitAfter
			if wait <= 0 {
				wait = 5 * time.Minute
			}
			if call.State == StatePending && !call.At.Add(wait).After(time.Now()) {
				return tx.PutBinding(AttentionKind, call.ID, call.ID)
			}
			return nil
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

type Decision struct {
	Revision uint64 `json:"revision"`
	Detail   string `json:"detail"`
}
type Reconciliation struct {
	Revision uint64          `json:"revision"`
	State    string          `json:"state"`
	Receipt  string          `json:"receipt"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	Detail   string          `json:"detail"`
}
type Resolution struct {
	Reconciliation
	Owner           string          `json:"owner"`
	At              time.Time       `json:"at"`
	OriginalState   string          `json:"original_state"`
	OriginalReceipt string          `json:"original_receipt,omitempty"`
	OriginalResult  json.RawMessage `json:"original_result,omitempty"`
	OriginalError   string          `json:"original_error,omitempty"`
}

func (b *Broker) Cancel(ctx context.Context, id string, input Decision) error {
	if len(input.Detail) > 4096 {
		return errors.New("cancellation detail exceeds its limit")
	}
	return b.Book.Update(ctx, func(tx *ledger.Tx) error {
		call, found, err := durable.Get[Invocation](tx, CallKind, id)
		if err != nil {
			return err
		}
		if !found {
			return ledger.ErrConflict
		}
		if call.State == StateCancelled && call.Resolution != nil && call.Resolution.Revision == input.Revision && call.Resolution.Detail == input.Detail {
			return nil
		}
		if call.Revision != input.Revision || call.State != StateApproval && call.State != StatePending {
			return ledger.ErrConflict
		}
		call.Resolution = &Resolution{Reconciliation: Reconciliation{Revision: input.Revision, State: StateCancelled, Detail: input.Detail}, Owner: b.Identity.OwnerID, At: time.Now().UTC(), OriginalState: call.State}
		call.State = StateCancelled
		call.Error = input.Detail
		return saveCall(tx, call)
	})
}

// Reconcile is owner evidence, not a new client invocation. Keep the original
// token, generation, arguments, contract and earlier unknown-result evidence.
func (b *Broker) Reconcile(ctx context.Context, id string, input Reconciliation) error {
	if input.State != StateDone && input.State != StateFailed || input.Receipt == "" || len(input.Receipt) > 4096 || len(input.Detail) > 4096 || len(input.Error) > 4096 || len(input.Result) > 256<<10 || len(input.Result) > 0 && !json.Valid(input.Result) {
		return errors.New("invalid client reconciliation")
	}
	return b.Book.Update(ctx, func(tx *ledger.Tx) error {
		call, found, err := durable.Get[Invocation](tx, CallKind, id)
		if err != nil {
			return err
		}
		if !found {
			return ledger.ErrConflict
		}
		if call.Resolution != nil && reflect.DeepEqual(call.Resolution.Reconciliation, input) {
			return nil
		}
		if call.State != StateUnknown || call.Revision != input.Revision {
			return ledger.ErrConflict
		}
		call.Resolution = &Resolution{Reconciliation: input, Owner: b.Identity.OwnerID, At: time.Now().UTC(), OriginalState: call.State, OriginalReceipt: call.Receipt, OriginalResult: append(json.RawMessage(nil), call.Result...), OriginalError: call.Error}
		call.State = input.State
		call.Result = input.Result
		call.Receipt = input.Receipt
		call.Error = input.Error
		if err := saveCall(tx, call); err != nil {
			return err
		}
		raw, _ := json.Marshal(struct {
			Invocation string      `json:"invocation"`
			Resolution *Resolution `json:"resolution"`
		}{id, call.Resolution})
		_, err = events.AppendTx(tx, events.Event{Source: "server:client", Key: id + ":reconciled", Type: "client.reconciled", Payload: raw, Subject: call.Request.Conversation, Depth: call.Depth, At: time.Now().UTC()})
		return err
	})
}
