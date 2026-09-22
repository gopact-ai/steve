package app

import (
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

// sessionBinding keeps normal session requests and stop observations tied to
// the same committed identity. Callers must first require an execution token.
func sessionBinding(record attempt.Record, tracked task.Task) nodewire.SessionBinding {
	return nodewire.SessionBinding{
		NativeImportID:  record.NativeImportID(),
		PluginRuntimeID: record.PluginRuntimeID(),
		ProjectID:       record.Project,
		SessionID:       attempt.RetainedSessionID(tracked.Channel, tracked.ID, record.Agent),
		TaskID:          record.TaskID,
		AttemptID:       record.ID,
		NodeID:          record.Node,
		ExecutionEpoch:  attempt.SessionExecutionEpoch(record),
		TaskEpoch:       record.Execution.Epoch,
	}
}
