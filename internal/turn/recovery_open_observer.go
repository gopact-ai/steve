package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
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
		return retainedBlocked("native-open", "检查原节点的会话查询接口", "当前节点暂不支持查询原会话创建结果。", "没有会话标识不代表没有创建会话。", "建议升级并连接原节点后重新检查，或取消原任务并等待节点确认。", harness.ErrStopUnconfirmed)
	}
	ctx = execution.WithProbeKey(ctx, execution.Key{TaskID: record.TaskID, InstanceID: record.TurnID, AttemptID: record.ID})
	state, err := recovery.ReconcileNodeOpen(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Workspace.Path, false)
	if err != nil {
		return retainedBlocked("native-open", "按原执行和会话创建指令查询原节点", "暂时无法取得原会话的创建记录。", "节点可能离线，或创建请求尚未到达；记录缺失不能证明原请求已取消。", "建议恢复原节点后重新检查；若不再继续，可点击停止，系统会封存原创建指令并核实取消结果。", err)
	}
	if state.OpenReceipt != nil && state.OpenReceipt.CancelledBeforeOpen {
		return retainedBlocked("native-open-cancelled", "读取节点持久保存的原创建指令取消记录", "原会话创建已取消，未向 Agent 发送任务输入。", "节点已阻止原创建请求随后启动，取消结果正在核对并写回任务。", "建议点击停止完成任务取消；确认后可以重新发起任务。", nil)
	}
	if state.Command == nil && state.InputAccepted == 0 {
		return retainedBlocked("native-open-found", "按原创建指令找回并读取同一个节点会话", "已找到原会话，但尚未向 Agent 发送任务输入。", "创建响应中断发生在输入发送前，原准备过程没有保存可准确重放的完整输入。", "建议点击停止，安全取消原准备；收到节点停止确认后重新发起任务。", nil)
	}
	return retainedBlocked("native-open-state", "按原创建指令找回原节点会话并核对输入回执", "原会话的输入状态与任务准备记录不一致。", "不能把这次查询变成新的执行，也不能重发可能已接受的输入。", "建议保留原记录继续核对，或点击停止并等待节点确认原执行停止。", harness.ErrStopUnconfirmed)
}
