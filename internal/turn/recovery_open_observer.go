package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func pendingNodeOpen(record attempt.Record, err error) *execution.NodePreparationObserverDetached {
	var pending *harness.NodeSessionOpenUncertain
	if !errors.As(err, &pending) || record.ID == "" || record.Node == "" || record.Execution == nil {
		return nil
	}
	binding := pending.Binding
	if binding.PluginRuntimeID != record.PluginRuntimeID() || binding.AttemptID != record.ID || binding.TaskID != record.TaskID || binding.ProjectID != record.Project || binding.NodeID != record.Node || binding.TaskEpoch != record.Execution.Epoch || binding.ExecutionEpoch != attempt.SessionExecutionEpoch(record) || pending.OpenCommandID != attempt.InputCommandID(record)+"/open" {
		return nil
	}
	return &execution.NodePreparationObserverDetached{AttemptID: record.ID, NodeID: record.Node, OpenCommandID: pending.OpenCommandID, Cause: err}
}

func pendingChatOpen(record attempt.Record) bool {
	return record.Kind == attempt.KindChat && attempt.PendingSessionOpen(record)
}

type originalOpenRecovery interface {
	ReconcileNodeOpen(context.Context, harness.Placement, string, bool) (nodewire.SessionState, error)
}

func (c *Coordinator) inspectPendingOpen(ctx context.Context, record attempt.Record) error {
	recovery, ok := c.runtime.(originalOpenRecovery)
	if !ok {
		return c.retainedBlocked("native-open", i18n.RetainedTriedSessionQuery, i18n.RetainedProblemOpenQueryUnsupported, c.text.T(i18n.RetainedReasonNoSessionID), i18n.RetainedAdviceUpgradeNode, harness.ErrStopUnconfirmed)
	}
	ctx = execution.WithProbeKey(ctx, execution.Key{TaskID: record.TaskID, InstanceID: record.TurnID, AttemptID: record.ID})
	state, err := recovery.ReconcileNodeOpen(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Workspace.Path, false)
	if errors.Is(err, harness.ErrNodeOpenAbsent) {
		// The node replied. It writes its record before starting anything,
		// so no Agent is running for this open and no task input was ever
		// sent. Sealing it is the move that ends the turn cleanly.
		return c.retainedBlocked("native-open-absent", i18n.RetainedTriedQueriedOpen, i18n.RetainedProblemOpenAbsent, c.text.T(i18n.RetainedReasonOpenNeverStarted), i18n.RetainedAdviceSealOpen, err)
	}
	if err != nil {
		return c.retainedBlocked("native-open", i18n.RetainedTriedQueryOpen, i18n.RetainedProblemOpenUnreadable, c.text.T(i18n.RetainedReasonOpenUnreached), i18n.RetainedAdviceRestoreOrStop, err)
	}
	if state.OpenReceipt != nil && state.OpenReceipt.CancelledBeforeOpen {
		return c.retainedBlocked("native-open-cancelled", i18n.RetainedTriedReadOpenCancel, i18n.RetainedProblemOpenCancelled, c.text.T(i18n.RetainedReasonOpenBlocked), i18n.RetainedAdviceStopToFinish, nil)
	}
	if state.Command == nil && state.InputAccepted == 0 {
		return c.retainedBlocked("native-open-found", i18n.RetainedTriedFindOpenSession, i18n.RetainedProblemOpenFound, c.text.T(i18n.RetainedReasonOpenNoInput), i18n.RetainedAdviceStopPreparation, nil)
	}
	return c.retainedBlocked("native-open-state", i18n.RetainedTriedCheckOpenReceipt, i18n.RetainedProblemOpenMismatch, c.text.T(i18n.RetainedReasonNoReplay), i18n.RetainedAdviceKeepOrStop, harness.ErrStopUnconfirmed)
}
