package cluster

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func sessionActionMode(action nodewire.SessionAction) (observation, stopping bool, err error) {
	switch action {
	case nodewire.SessionActionOpen, nodewire.SessionActionAttach, nodewire.SessionActionPoll, nodewire.SessionActionSettings, nodewire.SessionActionInspectOpen:
		return true, false, nil
	case nodewire.SessionActionCancel, nodewire.SessionActionAbort, nodewire.SessionActionClose, nodewire.SessionActionCancelOpen:
		return false, true, nil
	case nodewire.SessionActionStart, nodewire.SessionActionPrompt, nodewire.SessionActionAnswer, nodewire.SessionActionOption, nodewire.SessionActionCapabilities:
		return false, false, nil
	default:
		return false, false, errors.New("unsupported node session action")
	}
}

// Runtime scope checks query another owner, so they cannot nest inside the
// single-connection read transaction. Check them first, then re-read the
// execution and its authority together; never use a preflight's task/lease facts
// to authorize work after the potentially slower scope check.
func authorizeSessionRead(ctx context.Context, book *ledger.Ledger, binding nodewire.SessionBinding, action nodewire.SessionAction, checkRuntime func(attempt.Record) error) error {
	_, stopping, err := sessionActionMode(action)
	if err != nil {
		return err
	}
	var preflight attempt.Record
	if !stopping {
		if err := book.Read(ctx, func(tx *ledger.ReadTx) error {
			var err error
			preflight, err = attempt.GetTx(tx, binding.AttemptID)
			return err
		}); err != nil {
			return err
		}
		if preflight.PluginRuntimeID() != binding.PluginRuntimeID {
			return errors.New("node session differs from the committed runtime")
		}
		if err := checkRuntime(preflight); err != nil {
			return err
		}
	}
	authorized, err := readSessionExecution(ctx, book, binding, action)
	if err != nil {
		return err
	}
	if !stopping && !reflect.DeepEqual(preflight.PluginRuntime, authorized.PluginRuntime) {
		return errors.New("node session runtime changed during scope check")
	}
	return nil
}

// All execution facts are read in one snapshot. This runs after the peer has
// waited for the coordinator's committed ledger version, including on followers.
func readSessionExecution(ctx context.Context, book *ledger.Ledger, binding nodewire.SessionBinding, action nodewire.SessionAction) (attempt.Record, error) {
	observation, stopping, err := sessionActionMode(action)
	if err != nil {
		return attempt.Record{}, err
	}
	var record attempt.Record
	err = book.Read(ctx, func(tx *ledger.ReadTx) error {
		var err error
		record, err = attempt.GetTx(tx, binding.AttemptID)
		if err != nil {
			return err
		}
		if record.ID != binding.AttemptID || record.NativeImportID() != binding.NativeImportID || record.PluginRuntimeID() != binding.PluginRuntimeID || record.TaskID != binding.TaskID || record.Project != binding.ProjectID || record.Node != binding.NodeID || attempt.SessionExecutionEpoch(record) != binding.ExecutionEpoch || record.Execution == nil || record.Execution.TaskID != record.TaskID || record.Execution.Epoch != binding.TaskEpoch {
			return errors.New("node session differs from the committed execution")
		}
		tracked, found, err := task.GetTx(tx, record.TaskID)
		if err != nil {
			return err
		}
		if !found || binding.SessionID != LogicalAgentSession(tracked.Channel, tracked.ID, record.Agent) {
			return errors.New("node session conversation differs")
		}
		if stopping || observation {
			return nil
		}
		if record.State.Terminal() || record.Unsettled {
			return fmt.Errorf("execution %s cannot start more work", record.ID)
		}
		if action == nodewire.SessionActionPrompt && record.State != attempt.Running {
			return errors.New("native input requires a committed running attempt")
		}
		if action == nodewire.SessionActionStart && record.State != attempt.Leased && record.State != attempt.Prepared && record.State != attempt.Running {
			return errors.New("native session creation is outside the execution preparation phase")
		}
		if err := task.CheckExecutionTx(tx, record.Execution); err != nil {
			return err
		}
		for _, granted := range record.Leases {
			current, exists, err := tx.LeaseOf(granted.Key)
			if err != nil {
				return err
			}
			if !exists || current.Incarnation != granted.Incarnation || current.Epoch != granted.Epoch || current.Holder != granted.Holder || !current.ExpiresAt.After(time.Now()) {
				return ledger.ErrStale
			}
		}
		return nil
	})
	return record, err
}
