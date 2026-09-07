package turn

import (
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
)

func pendingNodeOpen(record attempt.Record, err error) *execution.NodePreparationObserverDetached {
	var pending *harness.NodeSessionOpenUncertain
	if !errors.As(err, &pending) || record.ID == "" || record.Node == "" || record.Execution == nil {
		return nil
	}
	binding := pending.Binding
	if binding.AttemptID != record.ID || binding.TaskID != record.TaskID || binding.ProjectID != record.Project || binding.NodeID != record.Node || binding.TaskEpoch != record.Execution.Epoch || binding.ExecutionEpoch != attempt.SessionExecutionEpoch(record) || pending.OpenCommandID != attempt.InputCommandID(record)+"/open" {
		return nil
	}
	return &execution.NodePreparationObserverDetached{AttemptID: record.ID, NodeID: record.Node, OpenCommandID: pending.OpenCommandID, Cause: err}
}

func pendingChatOpen(record attempt.Record) bool {
	return record.Kind == attempt.KindChat && record.Execution != nil && record.Node != "" && record.Unsettled && !strings.HasPrefix(record.Session, "ns_") && (record.State == attempt.Leased || record.State == attempt.Prepared)
}
