package delegate

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func pendingDelegateOpen(record attempt.Record, tracked task.Task, cause error) *execution.NodePreparationObserverDetached {
	var pending *harness.NodeSessionOpenUncertain
	if !errors.As(cause, &pending) || record.Execution == nil || record.Execution.TaskID != tracked.ID || record.TaskID != tracked.ID || record.Node == "" {
		return nil
	}
	expected := nodewire.SessionBinding{ProjectID: record.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(record), TaskEpoch: record.Execution.Epoch}
	if pending.Binding != expected || pending.OpenCommandID != attempt.InputCommandID(record)+"/open" {
		return nil
	}
	return &execution.NodePreparationObserverDetached{AttemptID: record.ID, NodeID: record.Node, OpenCommandID: pending.OpenCommandID, Cause: cause}
}

func pendingDelegatePreparation(record attempt.Record) bool {
	return record.Execution != nil && record.Node != "" && record.Unsettled && !strings.HasPrefix(record.Session, "ns_") && (record.State == attempt.Leased || record.State == attempt.Prepared)
}

func (s *Service) reportDelegatePreparation(ctx context.Context, parent, tracked task.Task, record attempt.Record) {
	s.reportRecovery(ctx, questionBinding(parent, tracked, record), "native-open",
		"核对原子任务的创建记录和执行节点",
		"原生会话的创建回执尚未确认。",
		"节点可能已经创建了原会话，但当前没有可靠的会话标识；不能把连接中断当作没有执行，也不能重新创建来补回执。",
		"建议核对原节点服务及创建回执后重新检查；原任务、输入和预算继续保留，暂不发送新任务。")
}
