package delegate

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func pendingDelegateOpen(record attempt.Record, tracked task.Task, cause error) *execution.NodePreparationObserverDetached {
	var pending *harness.NodeSessionOpenUncertain
	if !errors.As(cause, &pending) || record.Execution == nil || record.Execution.TaskID != tracked.ID || record.TaskID != tracked.ID || record.Node == "" {
		return nil
	}
	expected := nodewire.SessionBinding{PluginRuntimeID: record.PluginRuntimeID(), ProjectID: record.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(record), TaskEpoch: record.Execution.Epoch}
	if pending.Binding != expected || pending.OpenCommandID != attempt.InputCommandID(record)+"/open" {
		return nil
	}
	return &execution.NodePreparationObserverDetached{AttemptID: record.ID, NodeID: record.Node, OpenCommandID: pending.OpenCommandID, Cause: cause}
}

func pendingDelegatePreparation(record attempt.Record) bool {
	return record.Execution != nil && record.Node != "" && record.Unsettled && !nodewire.IsManagedSession(record.Session) && (record.State == attempt.Leased || record.State == attempt.Prepared)
}

func (s *Service) reportDelegatePreparation(ctx context.Context, parent, tracked task.Task, record attempt.Record) {
	s.reportRecovery(ctx, questionBinding(parent, tracked, record), "native-open",
		i18n.DelegateRecoveryTriedNativeOpen,
		i18n.DelegateRecoveryProblemNativeOpen,
		i18n.DelegateRecoveryReasonNativeOpen,
		i18n.DelegateRecoveryAdviceNativeOpen)
}
