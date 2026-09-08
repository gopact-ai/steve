package turn

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

type uncertainOpenManager struct {
	*fakeManager
	attempts *attempt.Service
	calls    int
}

type inspectingOpenManager struct {
	*uncertainOpenManager
	inspections int
}

func (m *inspectingOpenManager) ReconcileNodeOpen(ctx context.Context, _ harness.Placement, _ string, cancelOpen bool) (nodewire.SessionState, error) {
	if cancelOpen {
		return nodewire.SessionState{}, errors.New("resume must not cancel the original open")
	}
	m.inspections++
	return nodewire.SessionState{ID: "ns_original", State: "idle", OpenReceipt: &nodewire.SessionOpenReceipt{Action: "inspect-open"}}, nil
}

func (m *uncertainOpenManager) OpenSession(ctx context.Context, place harness.Placement, upstream, workspace string, servers []acp.MCPServer) (harness.Runner, error) {
	m.calls++
	key, _ := execution.KeyOf(ctx)
	r, err := m.attempts.Get(ctx, key.AttemptID)
	if err != nil {
		return nil, err
	}
	return nil, &harness.NodeSessionOpenUncertain{Binding: nodewire.SessionBinding{ProjectID: r.Project, TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: r.Execution.Epoch}, OpenCommandID: attempt.InputCommandID(r) + "/open", Cause: io.ErrUnexpectedEOF}
}

func TestLostFreshNodeOpenKeepsOriginalTaskUnsettledWhileObserverCanExit(t *testing.T) {
	c, _, _, original, req := retainedChatFixture(t)
	if _, err := c.ResumeRetainedChat(t.Context(), original.ID, req); err != nil {
		t.Fatal(err)
	}
	if err := c.store.DeleteSession(req.ConversationID, original.Agent); err != nil {
		t.Fatal(err)
	}
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.SetExecution(execution.New(lifetime, c.tasks))
	manager := &uncertainOpenManager{fakeManager: &fakeManager{}, attempts: c.attempts}
	c.runtime = manager
	req.Input, req.MessageID = "next task instruction", "web-next"
	_, err := c.Handle(lifetime, req)
	var uncertain *harness.NodeSessionOpenUncertain
	if !errors.As(err, &uncertain) || manager.calls != 1 {
		t.Fatalf("fresh open error was lost or retried: %v calls=%d", err, manager.calls)
	}
	record, err := c.attempts.Get(t.Context(), uncertain.Binding.AttemptID)
	if err != nil || !pendingChatOpen(record) || record.Session != "" {
		t.Fatalf("uncertain fresh open not durably preserved: %+v %v", record, err)
	}
	tracked, _ := c.tasks.Get(record.TaskID)
	if !tracked.Attempts[len(tracked.Attempts)-1].Open() {
		t.Fatal("lost open fabricated task settlement")
	}
	items, err := c.RetainedChats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.AttemptID == record.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("uncertain open exchange cannot expose recovery question")
	}
	if _, err := c.ResumeRetainedChat(lifetime, record.ID, req); err == nil || manager.calls != 1 {
		t.Fatal("missing native identity silently replayed original task")
	}
	inspector := &inspectingOpenManager{uncertainOpenManager: manager}
	c.runtime = inspector
	_, err = c.ResumeRetainedChat(lifetime, record.ID, req)
	var question *RecoveryBlocked
	if !errors.As(err, &question) || !strings.Contains(question.Question.Message, "已找到原会话") || !strings.Contains(question.Question.Message, "停止") || inspector.inspections != 1 || manager.calls != 1 {
		t.Fatalf("found preparation did not ask for safe cancellation without re-opening: inspections=%d opens=%d err=%v", inspector.inspections, manager.calls, err)
	}
	if err := c.executions.Stop([]string{record.TaskID}, task.ErrExecutionStopped).Wait(t.Context()); err == nil {
		t.Fatal("explicit stop claimed unknown node process was stopped")
	}
	cancel()
	if err := c.executions.Shutdown(t.Context()); err != nil {
		t.Fatalf("lost open blocked instance replacement: %v", err)
	}
}
