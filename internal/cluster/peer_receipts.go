package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

var ErrNodeReceiptPending = errors.New("node receipt lacks exact committed result, accounting or delivery")

// accountedEnding says whether a chat attempt's accounting records how it
// ended: a Bound attempt answered, so ok; a Failed or BindConflict one did
// not, and failed, was cancelled or timed out.
func accountedEnding(state attempt.State, outcome task.Outcome) bool {
	if state == attempt.Bound {
		return outcome == task.OutcomeOK
	}
	switch outcome {
	case task.OutcomeError, task.OutcomeCancelled, task.OutcomeTimeout:
		return true
	}
	return false
}

// ReadNodeReceiptProof joins owner readers under one committed local snapshot.
// Callers on a follower must first wait for the required committed version.
// Unsupported delivery owners, unknown usage and incomplete settlement retain
// evidence; client fields and the task's latest attempt cannot authorize GC.
// A task proved deleted leaves nothing to account or deliver, so its exact,
// settled terminal receipt is released.
func ReadNodeReceiptProof(ctx context.Context, book *ledger.Ledger, receipt nodewire.SessionReceipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	return book.Read(ctx, func(tx *ledger.ReadTx) error {
		if err := ledger.CheckOperationEnvelopesTx(tx, "attempt"); err != nil {
			return err
		}
		record, err := attempt.GetTx(tx, receipt.Binding.AttemptID)
		if err != nil {
			return err
		}
		if record.NodeReceipt == nil || *record.NodeReceipt != receipt || record.Kind != attempt.KindChat ||
			!record.State.Terminal() || record.Unsettled || record.SessionSettled == nil || !*record.SessionSettled ||
			record.EndedAt.IsZero() {
			return ErrNodeReceiptPending
		}
		if err := attempt.CheckNodeReceiptTx(tx, record, receipt); errors.Is(err, attempt.ErrNodeReceiptTaskDeleted) {
			// The conversation was deleted with its task, accounting and
			// delivery: there is no owner left to wait for, and the exact
			// settled terminal receipt above is all that remains to release.
			return nil
		} else if err != nil {
			return err
		}
		switch record.State {
		case attempt.Bound:
			var result turn.Result
			if record.Result == nil || json.Unmarshal(record.Result.Output, &result) != nil || result.Attempt != record.ID {
				return ErrNodeReceiptPending
			}
		case attempt.Failed, attempt.BindConflict:
			if record.Error == "" {
				return ErrNodeReceiptPending
			}
		default:
			return ErrNodeReceiptPending
		}
		u := record.Usage
		if u == nil || u.Input < 0 || u.Output < 0 || u.CachedRead < 0 || u.CachedWrite < 0 ||
			!u.Reported && u.Input == 0 && u.Output == 0 && u.CachedRead == 0 && u.CachedWrite == 0 {
			return ErrNodeReceiptPending
		}
		accounting, _, found, err := task.ReadAccountingTx(tx, record.TaskID, record.ID, record.TurnID)
		if err != nil {
			return err
		}
		// Chat binds the primary row admitted for this exact worker and node;
		// independent plan/verification accounting cannot stand in for it.
		if !found || accounting.Independent || accounting.Node != record.Node || accounting.Member != record.Agent ||
			accounting.ExecutionEpoch != record.Execution.Epoch || accounting.EndedAt.IsZero() ||
			!accountedEnding(record.State, accounting.Outcome) || accounting.UsageKnown == nil || !*accounting.UsageKnown ||
			accounting.Model != u.Model || accounting.Tokens != (task.Tokens{Input: u.Input, Output: u.Output,
			CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output}) {
			return ErrNodeReceiptPending
		}
		// GetTx is the task owner's header-only port, never Store.Get/history.
		tracked, found, err := task.GetTx(tx, record.TaskID)
		if err != nil {
			return err
		}
		if !found {
			return ErrNodeReceiptPending
		}
		var confirmed bool
		switch tracked.Transport {
		case "console":
			_, confirmed, err = console.ConfirmedAttemptDeliveryTx(tx, record.ID, tracked.Channel, record.TurnID)
		case "feishu":
			_, confirmed, err = gateway.ConfirmedAttemptDeliveryTx(tx, record.ID, tracked.ID, tracked.Channel, record.TurnID, tracked.Requester)
		default:
			return ErrNodeReceiptPending
		}
		if err != nil {
			return err
		}
		if !confirmed {
			return ErrNodeReceiptPending
		}
		return nil
	})
}

func (p *Peer) AuthorizeNodeReceipt(ctx context.Context, authenticatedNode string, authority nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
	if authenticatedNode != authority.CoordinatorNodeID {
		return errors.New("receipt coordinator differs from the authenticated peer")
	}
	return p.authorizeNodeReceipt(ctx, p.Config.NodeID, authority, receipt)
}

func (p *Peer) ApplicationReceiptAuthorizer(active Activation) func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionReceipt) error {
	return func(ctx context.Context, node string, authority nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
		if err := active.Context.Err(); err != nil {
			return err
		}
		if authority.CoordinatorNodeID != active.NodeID || authority.CoordinatorEpoch != active.Assignment.Epoch || authority.WriterGeneration != active.WriterGeneration {
			return coordination.ErrStaleEpoch
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(active.Context, cancel)
		defer stop()
		if err := p.authorizeNodeReceipt(ctx, node, authority, receipt); err != nil {
			return err
		}
		return active.Context.Err()
	}
}

func (p *Peer) authorizeNodeReceipt(ctx context.Context, node string, authority nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
	if authority.ClusterID != p.Config.ClusterID || receipt.Binding.NodeID != node {
		return errors.New("receipt belongs to another cluster or node")
	}
	runtime := p.Runtime.Load()
	if runtime == nil {
		return coordination.ErrUnavailable
	}
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return err
	}
	if state.Coordinator.NodeID != authority.CoordinatorNodeID || state.Coordinator.Epoch != authority.CoordinatorEpoch || state.WriterGeneration != authority.WriterGeneration {
		return coordination.ErrStaleEpoch
	}
	var poll *time.Ticker
	for {
		version, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return err
		}
		if version >= state.AppVersion {
			break
		}
		if poll == nil {
			poll = time.NewTicker(5 * time.Millisecond)
			defer poll.Stop()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
	if err := ReadNodeReceiptProof(ctx, runtime.Ledger(), receipt); err != nil {
		return fmt.Errorf("authorize exact native receipt: %w", err)
	}
	return ctx.Err()
}
