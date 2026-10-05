package turn

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func suppressedCompletionCoordinator(t *testing.T) (*Coordinator, *ledger.Ledger, task.Task, task.ExecutionToken) {
	t.Helper()
	runner := &fakeRunner{reply: "accepted", started: make(chan struct{}), done: make(chan struct{})}
	c, book := completionCoordinator(t, runner)
	release := sync.OnceFunc(func() { close(runner.done) })
	defer release()
	finished := make(chan error, 1)
	go func() { _, err := handle(c, t.Context(), "work"); finished <- err }()
	select {
	case <-runner.started:
	case err := <-finished:
		t.Fatalf("root turn did not start: %v", err)
	case <-time.After(waitDeadline):
		t.Fatal("root turn did not start")
	}
	p, _, err := c.projects.Get(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Home.Path, "final.txt"), []byte("accepted result\n"), 0600); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("root turn did not finish")
	}
	child, err := c.tasks.Spawn("1", task.Task{Member: "child", Origin: "delegate:1"})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := c.tasks.Spawn(child.ID, task.Task{Member: "grandchild", Origin: "delegate:" + child.ID})
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.tasks.ExecutionToken(grandchild.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []task.Task{child, grandchild} {
		if _, err := c.tasks.Begin(member.ID, member.Member, "hub", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := c.tasks.Finish(member.ID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := c.tasks.Advance(member.ID, task.StateDone); err != nil {
			t.Fatal(err)
		}
		if err := c.tasks.SetResult(member.ID, task.Result{Outcome: task.OutcomeOK, Answer: "accepted"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.tasks.SetDelivery(child.ID, task.DeliveryDelivered); err != nil {
		t.Fatal(err)
	}
	if err := c.tasks.SuppressDelivery(grandchild.ID, "parent ended"); err != nil {
		t.Fatal(err)
	}
	grandchild, _ = c.tasks.Get(grandchild.ID)
	return c, book, grandchild, token
}

func TestSuppressedDescendantCompletionStillRequiresDurableGuards(t *testing.T) {
	for _, scenario := range []string{"idle", "reserved", "running", "session-unknown", "session-unsettled", "quarantined", "registry-active", "registry-unresolved", "close-owed", "close-owed-other-task", "landing-queued", "landing-wal", "wrong-execution"} {
		t.Run(scenario, func(t *testing.T) {
			c, book, grandchild, token := suppressedCompletionCoordinator(t)
			var scope *execution.Scope
			var owed state.OwedClose
			want := task.ErrCompleteBusy
			begin := func(kind, state string, value any) {
				t.Helper()
				if _, err := book.Begin(t.Context(), "pending", kind, state, "test", value); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "idle":
				want = nil
			case "reserved", "running", "session-unknown", "session-unsettled", "quarantined":
				settled := true
				r := attempt.Record{Spec: attempt.Spec{ID: "pending", TaskID: grandchild.ID, Execution: &token}, SessionSettled: &settled}
				state := "bound"
				switch scenario {
				case "reserved":
					state = "leased"
				case "running":
					state = "running"
				case "session-unknown":
					r.SessionSettled = nil
				case "session-unsettled":
					settled = false
				case "quarantined":
					r.Unsettled = true
				}
				begin("attempt", state, r)
			case "registry-active", "registry-unresolved":
				var err error
				scope, err = c.executions.BeginAccepted(t.Context(), execution.Key{TaskID: grandchild.ID, InstanceID: "writer"}, &token)
				if err != nil {
					t.Fatal(err)
				}
				defer scope.Finish(nil)
				if scenario == "registry-unresolved" {
					scope.Finish(errors.New("writer exit not confirmed"))
				}
			case "close-owed", "close-owed-other-task":
				session := state.Session{ConversationID: "child-chat", AgentID: "grandchild", NodeID: "node", HarnessID: "codex", UpstreamID: "ns_child"}
				if err := c.store.SaveSession(session); err != nil {
					t.Fatal(err)
				}
				owed = state.OwedClose{NodeID: session.NodeID, HarnessID: session.HarnessID, UpstreamID: session.UpstreamID, TaskID: grandchild.ID, AttemptID: "pending", OwedAt: time.Now().UTC().Format(time.RFC3339Nano)}
				if scenario == "close-owed-other-task" {
					want = nil
					other, err := c.tasks.Create(task.Task{Channel: "other-chat", Member: "other"})
					if err != nil {
						t.Fatal(err)
					}
					owed.TaskID = other.ID
				}
				if err := c.store.ArchiveSessionOwingClose(session.ConversationID, session.AgentID, owed.OwedAt, owed); err != nil {
					t.Fatal(err)
				}
			case "landing-queued":
				want = task.ErrCompleteDelivery
				if err := book.PutBinding(t.Context(), "pending-landing", "p/final", artifact.Pending{Project: "p", Artifact: "final", Source: &artifact.Source{AttemptID: "producer", Execution: &token}}); err != nil {
					t.Fatal(err)
				}
			case "landing-wal":
				want = task.ErrCompleteDelivery
				begin("landing", artifact.LandApplying, artifact.Landing{ID: "pending", Project: "p", Artifact: "final", Source: &artifact.Source{AttemptID: "producer", Execution: &token}})
			case "wrong-execution":
				want = task.ErrExecutionStopped
				other, err := task.OpenLedger(book)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := other.SetAside("1", task.StatePaused); err != nil {
					t.Fatal(err)
				}
				if _, err := other.Advance("1", task.StateRunning); err != nil {
					t.Fatal(err)
				}
			}
			before := c.tasks.List("chat")
			var ids map[string]bool
			for _, root := range before {
				if root.ID == "1" && task.CompletionBlocker(root, before) != nil {
					t.Fatal("test did not reach the independent completion guards")
				}
			}
			complete := func() error {
				_, err := c.tasks.CompleteRoot(t.Context(), "1", "chat", func(tx *ledger.Tx, tree map[string]bool) error {
					ids = tree
					return c.checkTaskCompletionTx(tx, tree, "chat", "", false)
				})
				return err
			}
			if err := c.executions.WhileTaskIdle("1", complete); !errors.Is(err, want) {
				t.Fatalf("complete with %s = %v, want %v", scenario, err, want)
			}
			if want != nil {
				if !reflect.DeepEqual(before, c.tasks.List("chat")) {
					t.Fatal("refusal changed tasks or revoked their epochs")
				}
				if scenario == "registry-active" && scope.Context().Err() != nil {
					t.Fatal("refused completion cancelled an active writer")
				}
				if scenario == "close-owed" && !reflect.DeepEqual(c.store.OwedCloses(), []state.OwedClose{owed}) {
					t.Fatal("refused completion forgot the close obligation")
				}
				return
			}
			if !ids[grandchild.ID] || !errors.Is(c.tasks.CheckExecution(token), task.ErrExecutionStopped) {
				t.Fatal("completion lost the descendant or kept its execution grant")
			}
			after, _ := c.tasks.Get(grandchild.ID)
			if !reflect.DeepEqual(grandchild.Result, after.Result) || !reflect.DeepEqual(grandchild.Delivery, after.Delivery) || !reflect.DeepEqual(grandchild.Attempts, after.Attempts) {
				t.Fatal("completion changed the suppressed result or accounting")
			}
		})
	}
}

func TestCompleteCommandAcceptsSuppressedDescendantWithoutChangingFinalArtifact(t *testing.T) {
	c, _, grandchild, _ := suppressedCompletionCoordinator(t)
	conversation := c.store.Conversation("chat")
	before, err := c.attempts.ForTask(t.Context(), "1")
	if err != nil || len(before) != 1 || before[0].Result == nil || before[0].Result.Artifact == "" {
		t.Fatalf("root has no committed final artifact: %+v, %v", before, err)
	}
	for range 2 {
		result, err := handle(c, t.Context(), "/tasks complete 1")
		if err != nil || !strings.Contains(result.Text, "completed") {
			t.Fatalf("complete command with suppressed descendant: %+v, %v", result, err)
		}
	}
	after, err := c.attempts.ForTask(t.Context(), "1")
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(conversation, c.store.Conversation("chat")) {
		t.Fatal("completion changed the final artifact, execution record or native context")
	}
	root, _ := c.tasks.Get("1")
	if root.State != task.StateDone || !root.CompletedByUser {
		t.Fatal("command did not durably complete the root")
	}
	closed, _ := c.tasks.Get(grandchild.ID)
	if !reflect.DeepEqual(grandchild.Result, closed.Result) || !reflect.DeepEqual(grandchild.Delivery, closed.Delivery) {
		t.Fatal("command rewrote the suppressed result or claimed it was delivered")
	}
}
