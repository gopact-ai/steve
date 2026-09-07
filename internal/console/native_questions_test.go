package console

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

type nativeQuestionTransport struct {
	mu         sync.Mutex
	state      nodewire.SessionState
	permission bool
	answer     *nodewire.SessionAnswer
	changed    chan struct{}
}

func (*nativeQuestionTransport) Transport(string, string) acphost.Transport {
	panic("native question test must not open raw ACP")
}
func (tr *nativeQuestionTransport) NodeSession(ctx context.Context, _ string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	tr.mu.Lock()
	if req.Action == "poll" && req.After >= tr.state.Sequence {
		changed := tr.changed
		tr.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nodewire.SessionState{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		tr.mu.Lock()
	}
	defer tr.mu.Unlock()
	switch req.Action {
	case "open":
		tr.state = nodewire.SessionState{ID: "ns_" + strings.Repeat("a", 64), Binding: req.Binding, State: "idle", Sequence: 1}
	case "prompt":
		tr.state.State = "running"
		tr.state.InputAccepted = 1
		tr.state.Command = &nodewire.SessionCommand{ID: req.CommandID, InputSequence: 1, State: "running"}
		tr.state.Sequence++
		q := nodewire.SessionQuestion{ID: "nq_" + strings.Repeat("b", 64), CommandID: req.CommandID, State: "pending", Question: view.Question{SessionID: "native-session", RequestID: "nq_" + strings.Repeat("b", 64), Message: "I checked the network and could not connect. How should I continue?", AllowFreeText: true}}
		if tr.permission {
			q.Question.Kind = "permission"
			q.Question.AllowFreeText = false
			q.Permission = &permission.Ask{SessionID: "native-session", ToolCallID: "tool-1", ToolName: "Modify project configuration", Reason: "The configuration must be updated.", Options: []acp.PermissionOption{{OptionID: "allow", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce}, {OptionID: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce}}}
		}
		tr.state.Questions = []nodewire.SessionQuestion{q}
	case "answer":
		answer := *req.Answer
		tr.answer = &answer
		tr.state.Questions[0].State = "answered"
		tr.state.Questions[0].Answer = &answer
		tr.state.Command = &nodewire.SessionCommand{ID: tr.state.Command.ID, InputSequence: 1, State: "completed", Settled: true, Output: "continued original child"}
		tr.state.State = "idle"
		tr.state.Sequence++
		close(tr.changed)
		tr.changed = make(chan struct{})
	}
	raw, _ := json.Marshal(tr.state)
	var out nodewire.SessionState
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func TestNativeDelegateQuestionsKeepOriginalChildBindingAndParentConversation(t *testing.T) {
	for _, isPermission := range []bool{false, true} {
		t.Run(map[bool]string{false: "question", true: "permission"}[isPermission], func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			service := New(&echo{}, "owner", nil)
			if err := service.Persist(book.Document("console")); err != nil {
				t.Fatal(err)
			}
			transport := &nativeQuestionTransport{permission: isPermission, changed: make(chan struct{})}
			manager, _ := harness.NewManager(nil)
			manager.SetTransports(transport)
			defer manager.Stop()
			binding := nodewire.SessionBinding{ProjectID: "p", SessionID: "parent-child-agent", TaskID: "child-task", AttemptID: "child-attempt", NodeID: "worker", ExecutionEpoch: 1, TaskEpoch: 1}
			ctx, cancel := context.WithTimeout(harness.WithNodeSession(t.Context(), harness.NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "cluster", CoordinatorNodeID: "coordinator", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: binding, CommandID: "child-input"}), 2*time.Second)
			defer cancel()
			runner, err := manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			base := consoleapi.PendingQuestion{Conversation: "console:parent", Project: "p", TaskID: binding.TaskID, AttemptID: binding.AttemptID, SessionID: runner.ID(), RequestID: "nq_" + strings.Repeat("b", 64), Principal: "forged-owner", AllowFreeText: true}
			done := make(chan error, 1)
			go func() {
				_, _, err := runner.(harness.TurnRunner).PromptTurn(ctx, "original child", nil,
					func(ctx context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
						bad := base
						bad.AttemptID = "other-attempt"
						if _, err := service.RequestNativePermission(ctx, bad, ask); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
							t.Error("permission accepted wrong attempt", err)
						}
						return service.RequestNativePermission(ctx, base, ask)
					},
					func(ctx context.Context, q view.Question) (view.Answer, error) {
						for _, bad := range []consoleapi.PendingQuestion{
							{Conversation: base.Conversation, Project: "other-project", TaskID: base.TaskID, AttemptID: base.AttemptID, SessionID: base.SessionID, RequestID: base.RequestID},
							{Conversation: base.Conversation, Project: base.Project, TaskID: "other-task", AttemptID: base.AttemptID, SessionID: base.SessionID, RequestID: base.RequestID},
							{Conversation: base.Conversation, Project: base.Project, TaskID: base.TaskID, AttemptID: base.AttemptID, SessionID: base.SessionID, RequestID: "nq_old"},
						} {
							if _, err := service.RequestNativeQuestion(ctx, bad, q); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
								t.Error("question accepted wrong native binding", err)
							}
						}
						if _, err := service.RequestNativeQuestion(context.Background(), base, q); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
							t.Error("question accepted forged callback context", err)
						}
						return service.RequestNativeQuestion(ctx, base, q)
					}, nil)
				done <- err
			}()
			q := pendingForTest(t, service)
			if q.Conversation != base.Conversation || q.TaskID != binding.TaskID || q.AttemptID != binding.AttemptID || q.RequestID != base.RequestID || q.Principal != "owner" || !q.Deadline.IsZero() {
				t.Fatalf("native parent question lost scope: %+v", q)
			}
			answer := consoleapi.QuestionAnswer{CommandID: "user-answer", Decision: "accept", Text: "Wait for me to reconnect the network."}
			if isPermission {
				if q.Kind != "permission" || q.AllowFreeText {
					t.Fatal("permission became free-text recovery approval")
				}
				if _, err := service.AnswerQuestion(ctx, q.ID, answer); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
					t.Fatal("text granted native permission", err)
				}
				answer.Text = ""
				answer.Choice = "allow"
			}
			if _, err := service.AnswerQuestion(ctx, q.ID, answer); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			transport.mu.Lock()
			got := *transport.answer
			transport.mu.Unlock()
			if got.Text != answer.Text || got.Choice != answer.Choice {
				t.Fatalf("native answer changed: %+v", got)
			}
			if len(service.exchanges) != 0 {
				t.Fatal("native question created another task submission")
			}
		})
	}
}
