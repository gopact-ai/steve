package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// applicationRecovery orders the existing authorities: persisted execution
// proof, adapter acceptance, exact accounting, and only then queue dispatch.
// It stores no recovery truth of its own. orphaned only distinguishes the
// tasks found at startup from pre-admission work alive in this generation.
type applicationRecovery struct {
	mu          sync.Mutex
	book        *ledger.Ledger
	attempts    *attempt.Service
	tasks       *task.Store
	coordinator *turn.Coordinator
	gateway     *gateway.Gateway
	console     *console.Service
	text        i18n.Catalog
	retained    bool
	orphaned    map[string]bool
	workers     *reconciliationWorkers
}

func newApplicationRecovery(book *ledger.Ledger, attempts *attempt.Service, tasks *task.Store, coordinator *turn.Coordinator, gateway *gateway.Gateway, console *console.Service, text i18n.Catalog, retained bool) *applicationRecovery {
	r := &applicationRecovery{book: book, attempts: attempts, tasks: tasks, coordinator: coordinator, gateway: gateway, console: console, text: text, retained: retained, orphaned: map[string]bool{}, workers: &reconciliationWorkers{}}
	for _, candidate := range tasks.OpenPrimaryAccounting() {
		if conversationDriven(candidate.Task) {
			r.orphaned[primaryRecoveryKey(candidate)] = true
		}
	}
	return r
}

// conversationDriven reports whether a task's open accounting is settled by
// continuing its conversation. A plan resumes through the coordinator and a
// delegated child through the delegate service's retained-execution
// recovery; their rows belong to a different attempt kind and are never a
// chat exchange to reconcile or re-prompt.
func conversationDriven(t task.Task) bool {
	return t.Origin != "plan" && !t.Delegated()
}

func primaryRecoveryKey(candidate task.RecoveryCandidate) string {
	return fmt.Sprintf("task-restart/%s/%d/%d", candidate.Task.ID, candidate.Attempt.ExecutionEpoch, candidate.Index)
}

func (r *applicationRecovery) Reconcile(ctx context.Context) error {
	return r.reconcile(ctx, true)
}

func (r *applicationRecovery) reconcile(ctx context.Context, deliver bool) error {
	if !r.mu.TryLock() {
		return nil
	}
	defer r.mu.Unlock()
	var failures error
	for _, candidate := range r.tasks.OpenPrimaryAccounting() {
		if !conversationDriven(candidate.Task) {
			continue
		}
		if err := r.reconcileTask(ctx, candidate); err != nil {
			failures = errors.Join(failures, fmt.Errorf("task %s recovery: %w", candidate.Task.ID, err))
		}
	}
	// Do not release any deferred inputs when one original settlement failed.
	// Already accepted inputs remain durably blocked, even if another exchange
	// finishes concurrently and tries to drain the ordinary queue.
	if failures != nil {
		return failures
	}
	if r.retained {
		if err := r.console.RecoverChats(ctx, r.coordinator); err != nil {
			return err
		}
	}
	if err := r.console.Drain(); err != nil {
		return err
	}
	if deliver && r.gateway != nil {
		return r.gateway.ReconcileQueued(ctx, r.book, r.coordinator, r.coordinator.ReviveSession, r.workers)
	}
	return nil
}

func (r *applicationRecovery) reconcileTask(ctx context.Context, candidate task.RecoveryCandidate) error {
	tracked, row := candidate.Task, candidate.Attempt
	if r.coordinator.ChatObserverActive(tracked.Channel, tracked.Member) {
		return nil
	}
	key := primaryRecoveryKey(candidate)
	if !row.Open() {
		return nil
	}
	var record attempt.Record
	if row.ExecutionID != "" {
		var err error
		record, err = r.attempts.Get(ctx, row.ExecutionID)
		if err != nil {
			return err
		}
		if record.Kind != attempt.KindChat || record.TaskID != tracked.ID || record.TurnID != row.TurnID || record.Execution == nil || record.Execution.TaskID != tracked.ID || record.Execution.Epoch != row.ExecutionEpoch {
			return errors.New("original attempt/accounting identity does not match")
		}
		if record.Unsettled || !record.State.Terminal() || record.SessionSettled == nil || !*record.SessionSettled {
			return nil
		}
		if record.State == attempt.Bound {
			if tracked.Transport == "feishu" {
				if err := r.queue(ctx, tracked, "task-result/"+record.ID, record.ID); err != nil {
					return err
				}
			}
			return r.coordinator.SettleChatAccounting(ctx, record.ID)
		}
		// Node-owned commands are observed or reported in their original exchange,
		// never automatically re-prompted because their task row remained open.
		if strings.HasPrefix(record.Session, "ns_") || record.State != attempt.Expired {
			return r.coordinator.SettleChatAccounting(ctx, record.ID)
		}
	} else {
		if !r.orphaned[key] {
			return nil
		}
		if grant := tracked.ResumeGrant; grant.Consumed && grant.TurnID == row.TurnID &&
			grant.Admission.TaskID == tracked.ID && grant.Admission.Epoch == row.ExecutionEpoch {
			return fmt.Errorf("%w: original resume input %s has no bound attempt proof", task.ErrResumeConsumed, grant.Admission.ID)
		}
		// BindAttempt precedes native preparation. An unbound startup row can
		// resume only when there is no separately admitted attempt for its input.
		if existing, found, err := r.attempts.LatestForTurn(ctx, tracked.AnchorMessage); err != nil {
			return err
		} else if found && existing.TaskID == tracked.ID {
			return errors.New("unbound accounting has a separately admitted execution")
		}
	}
	if !r.orphaned[key] {
		return nil
	}
	if tracked.State == task.StateRunning && time.Since(tracked.UpdatedAt) <= staleTask {
		if err := r.coordinator.ReviveSession(tracked.Channel, tracked.Member); err != nil {
			return err
		}
		if err := r.queue(ctx, tracked, key, ""); err != nil {
			return err
		}
	} else {
		slog.Info("retaining stopped task without new continuation", "task", tracked.ID, "state", tracked.State)
	}
	// The row was opened by a process that is gone. The only end its
	// execution has on record is when this process noticed it — after the
	// outage, which is not work — so the row ends when it began: the turn
	// stays charged, the downtime does not. A row that never reached an
	// admitted execution ran nothing at all.
	if record.ID != "" {
		return r.tasks.SettleAttempt(tracked.ID, record.ID, record.TurnID, row.StartedAt, task.OutcomeInterrupted, stoppedAccounting(record))
	}
	_, err := r.tasks.FinishUnstarted(tracked.ID, task.OutcomeInterrupted)
	return err
}

func (r *applicationRecovery) queue(ctx context.Context, t task.Task, key, completedAttempt string) error {
	switch t.Transport {
	case "console":
		return r.console.QueueTaskResume(ctx, t.Channel, t.ID, key, t.Member, r.text.T(i18n.ResumeNotice, t.ID), r.text.T(i18n.ResumePrompt, t.Goal))
	case "feishu":
		if r.gateway == nil {
			return errors.New("Feishu recovery delivery is not configured")
		}
		return r.gateway.QueueRecovery(ctx, r.book, key, gateway.Revival{TaskID: t.ID, Goal: t.Goal, Member: t.Member, ConversationID: t.Channel, ChatID: t.ChatID, MessageID: t.AnchorMessage, Requester: t.Requester, ChatType: t.ChatType, OpenCard: t.OpenCard, Interim: t.Interim}, completedAttempt)
	default:
		return fmt.Errorf("unsupported recovery transport %q", t.Transport)
	}
}
