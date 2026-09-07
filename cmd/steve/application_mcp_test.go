package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

type applicationMCPReplicator struct{ book *ledger.Ledger }

func (r applicationMCPReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	v, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: v, CoordinatorEpoch: 1}, err
}
func (r applicationMCPReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

func applicationGrantGate(t *testing.T, book *ledger.Ledger) *agentmcp.Server {
	t.Helper()
	gate, err := agentmcp.New(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.SetStore(newApplicationMCPStore(book), nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- gate.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return gate
}
func grantRPC(t *testing.T, gate *agentmcp.Server, token, method string) int {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"name": "steve_help", "arguments": map[string]any{}}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gate.URL(), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}
func openGrantAttempt(t *testing.T, book *ledger.Ledger, tasks *task.Store, id, session string) (attempt.Record, agentmcp.GrantScope) {
	t.Helper()
	work, err := tasks.Create(task.Task{Channel: "chat", Member: "agent", ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(work.ID, "agent", "node-a", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	service := attempt.New(book)
	record, err := service.Open(t.Context(), attempt.Spec{ID: id, TaskID: work.ID, TurnID: id + "-input", Execution: &token, Kind: attempt.KindChat, Project: "project", Node: "node-a", Agent: "agent", Harness: "test", Scope: attempt.ScopeNone, Workspace: project.Workspace{ID: id + "-workspace", Project: "project", Node: "node-a", Kind: project.KindWorktree, Path: filepath.Join(t.TempDir(), id)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Advance(t.Context(), record.ID, attempt.Prepared, "test", nil); err != nil {
		t.Fatal(err)
	}
	record, err = service.RecordSession(t.Context(), record.ID, "test", session)
	if err != nil {
		t.Fatal(err)
	}
	return record, agentmcp.GrantScope{TaskID: work.ID, TaskEpoch: token.Epoch, AttemptID: record.ID, ExecutionGeneration: attempt.SessionExecutionEpoch(record), NodeID: record.Node, SessionID: record.Session}
}
func applicationGrantBook(t *testing.T) (*ledger.Ledger, *task.Store) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	return book, tasks
}

func TestApplicationMCPGrantFollowsRealAttemptLifecycleAndReplicaSnapshot(t *testing.T) {
	book, tasks := applicationGrantBook(t)
	gate := applicationGrantGate(t, book)
	gate.Extras("chat", "agent", "original-native-token", "")
	if status := grantRPC(t, gate, "original-native-token", "tools/list"); status != http.StatusOK {
		t.Fatalf("new session metadata: %d", status)
	}
	record, scope := openGrantAttempt(t, book, tasks, "original", "ns_original")
	binding := agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}
	if err := gate.BindExecution(t.Context(), binding, scope); !errors.Is(err, agentmcp.ErrGrantDenied) {
		t.Fatalf("prepared tools grant: %v", err)
	}
	if status := grantRPC(t, gate, "original-native-token", "tools/call"); status != http.StatusUnauthorized {
		t.Fatalf("pre-prompt tools: %d", status)
	}
	service := attempt.New(book)
	if _, err := service.Advance(t.Context(), record.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := gate.BindExecution(t.Context(), binding, scope); err != nil {
		t.Fatal(err)
	}
	if status := grantRPC(t, gate, "original-native-token", "tools/call"); status != http.StatusOK {
		t.Fatalf("running tools: %d", status)
	}
	snapshot, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	replica, _ := applicationGrantBook(t)
	if err := replica.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := replica.AttachReplication(applicationMCPReplicator{replica}); err != nil {
		t.Fatal(err)
	}
	fresh := applicationGrantGate(t, replica)
	if err := fresh.BindExecution(t.Context(), binding, scope); err != nil {
		t.Fatalf("retained original: %v", err)
	}
	if status := grantRPC(t, fresh, "original-native-token", "tools/call"); status != http.StatusOK {
		t.Fatalf("migrated token: %d", status)
	}
	restoredTasks, err := task.OpenLedger(replica, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoredTasks.SetAside(scope.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := restoredTasks.Advance(scope.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	if status := grantRPC(t, fresh, "original-native-token", "tools/call"); status != http.StatusUnauthorized {
		t.Fatalf("old token upgraded after pause/resume: %d", status)
	}
	if err := fresh.BindExecution(t.Context(), binding, scope); !errors.Is(err, agentmcp.ErrGrantDenied) {
		t.Fatalf("cancelled rebind: %v", err)
	}
}

type grantMemoryWriter struct {
	service          *memory.Service
	started, release chan struct{}
}

func (m *grantMemoryWriter) Remember(ctx context.Context, conversation, agent, by, scope, section, text, key string) (memory.Receipt, memory.Scope, error) {
	close(m.started)
	select {
	case <-ctx.Done():
		return memory.Receipt{}, memory.Scope{}, ctx.Err()
	case <-m.release:
	}
	r, err := m.service.Remember(ctx, memory.Global, section, text, key, memory.Actor{By: "agent", Conversation: conversation, Agent: agent})
	return r, memory.Global, err
}
func (*grantMemoryWriter) Recall(context.Context, string, string, string, string, int) ([]memory.Hit, string, error) {
	return nil, "", nil
}
func (*grantMemoryWriter) Forget(context.Context, string, string, string, string, string) (memory.Item, error) {
	return memory.Item{}, errors.New("unused")
}

func TestApplicationMCPMemoryRechecksOriginalGrantInsideWriteTransaction(t *testing.T) {
	for _, stop := range []string{"pause_resume", "revoke"} {
		t.Run(stop, func(t *testing.T) {
			book, tasks := applicationGrantBook(t)
			gate := applicationGrantGate(t, book)
			gate.Extras("chat", "agent", "native-token", "")
			record, scope := openGrantAttempt(t, book, tasks, "original", "ns_original")
			if _, err := attempt.New(book).Advance(t.Context(), record.ID, attempt.Running, "test", nil); err != nil {
				t.Fatal(err)
			}
			if err := gate.BindExecution(t.Context(), agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
				t.Fatal(err)
			}
			profile, err := prepareApplicationMemory(t.Context(), &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(t.TempDir(), "state.json")}}, book)
			if err != nil {
				t.Fatal(err)
			}
			writer := &grantMemoryWriter{service: profile.Service, started: make(chan struct{}), release: make(chan struct{})}
			gate.SetMemorizer(writer)
			finished := make(chan string, 1)
			go func() {
				body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"steve_remember","arguments":{"scope":"global","section":"偏好","text":"unauthorized late fact","idempotency_key":"late"}}}`
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gate.URL(), strings.NewReader(body))
				if err != nil {
					finished <- err.Error()
					return
				}
				req.Header.Set("Authorization", "Bearer native-token")
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					finished <- err.Error()
					return
				}
				defer response.Body.Close()
				raw, _ := io.ReadAll(response.Body)
				finished <- string(raw)
			}()
			select {
			case <-writer.started:
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			if stop == "revoke" {
				gate.Revoke("native-token")
			} else {
				if _, err := tasks.SetAside(scope.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
				if _, err := tasks.Advance(scope.TaskID, task.StateRunning); err != nil {
					t.Fatal(err)
				}
			}
			close(writer.release)
			if body := <-finished; !strings.Contains(body, `"isError":true`) {
				t.Fatalf("late mutation acknowledged: %s", body)
			}
			items, err := profile.Shared.List(t.Context(), memory.Global)
			if err != nil || len(items) != 0 {
				t.Fatalf("old grant mutated memory: %+v %v", items, err)
			}
		})
	}
}

func TestApplicationMCPNormalNextTurnReusesTokenOnlyAfterBoundSameSession(t *testing.T) {
	book, tasks := applicationGrantBook(t)
	gate := applicationGrantGate(t, book)
	gate.Extras("chat", "agent", "original-native-token", "")
	binding := agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}
	original, scope := openGrantAttempt(t, book, tasks, "original", "ns_original")
	service := attempt.New(book)
	if _, err := service.Advance(t.Context(), original.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := gate.BindExecution(t.Context(), binding, scope); err != nil {
		t.Fatal(err)
	}
	if err := service.MarkSessionSettled(t.Context(), original.ID, "native completed"); err != nil {
		t.Fatal(err)
	}
	if err := gate.BindExecution(t.Context(), binding, scope); err != nil {
		t.Fatalf("retained completed result unavailable: %v", err)
	}
	if status := grantRPC(t, gate, "original-native-token", "tools/call"); status != http.StatusUnauthorized {
		t.Fatalf("settled native still executes: %d", status)
	}
	if _, err := service.Advance(t.Context(), original.ID, attempt.BindReady, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(t.Context(), original.ID, "test", attempt.Completion{Result: attempt.Result{Summary: "done"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Advance(scope.TaskID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	next, nextScope := openGrantAttempt(t, book, tasks, "next", "ns_original")
	if _, err := service.Advance(t.Context(), next.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := gate.BindExecution(t.Context(), binding, nextScope); err != nil {
		t.Fatalf("normal next user turn refused: %v", err)
	}
	if status := grantRPC(t, gate, "original-native-token", "tools/call"); status != http.StatusOK {
		t.Fatalf("next turn original token: %d", status)
	}
	if err := gate.BindExecution(t.Context(), binding, scope); !errors.Is(err, agentmcp.ErrGrantDenied) {
		t.Fatalf("late old bind downgraded token: %v", err)
	}
}

func TestApplicationMCPGrantRejectsChangedIdentityAndLostAttemptLease(t *testing.T) {
	book, tasks := applicationGrantBook(t)
	gate := applicationGrantGate(t, book)
	gate.Extras("chat", "agent", "native-token", "")
	record, scope := openGrantAttempt(t, book, tasks, "original", "ns_original")
	service := attempt.New(book)
	if _, err := service.Advance(t.Context(), record.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	b := agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}
	for _, changed := range []agentmcp.GrantScope{
		{TaskID: scope.TaskID, TaskEpoch: scope.TaskEpoch + 1, AttemptID: scope.AttemptID, ExecutionGeneration: scope.ExecutionGeneration, NodeID: scope.NodeID, SessionID: scope.SessionID},
		{TaskID: scope.TaskID, TaskEpoch: scope.TaskEpoch, AttemptID: scope.AttemptID, ExecutionGeneration: scope.ExecutionGeneration + 1, NodeID: scope.NodeID, SessionID: scope.SessionID},
		{TaskID: scope.TaskID, TaskEpoch: scope.TaskEpoch, AttemptID: scope.AttemptID, ExecutionGeneration: scope.ExecutionGeneration, NodeID: "other-node", SessionID: scope.SessionID},
		{TaskID: scope.TaskID, TaskEpoch: scope.TaskEpoch, AttemptID: scope.AttemptID, ExecutionGeneration: scope.ExecutionGeneration, NodeID: scope.NodeID, SessionID: "ns_replacement"},
	} {
		if err := gate.BindExecution(t.Context(), b, changed); !errors.Is(err, agentmcp.ErrGrantDenied) {
			t.Fatalf("mismatched identity accepted: %+v %v", changed, err)
		}
	}
	if err := gate.BindExecution(t.Context(), b, scope); err != nil {
		t.Fatal(err)
	}
	if err := book.Release(t.Context(), record.Leases[0]); err != nil {
		t.Fatal(err)
	}
	if status := grantRPC(t, gate, "native-token", "tools/call"); status != http.StatusUnauthorized {
		t.Fatalf("lost lease allowed tools: %d", status)
	}
}
