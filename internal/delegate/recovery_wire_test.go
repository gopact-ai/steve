package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type delegateNodeAuthority struct {
	mu    sync.Mutex
	epoch uint64
}

func (a *delegateNodeAuthority) AuthorizeNodeSession(_ context.Context, principal string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, _ nodewire.SessionAction) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if principal != "delegate-cluster" || authority.ClusterID != principal || authority.CoordinatorEpoch != a.epoch || authority.WriterGeneration != a.epoch || binding.NodeID != "node-a" {
		return errors.New("test authority differs")
	}
	return nil
}

type delegateWireCount struct {
	mu      sync.Mutex
	prompts int
}
type delegateTransport struct {
	*node.Registry
	count *delegateWireCount
}

func (r *delegateTransport) NodeSession(ctx context.Context, name string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	if req.Action == "prompt" {
		r.count.mu.Lock()
		r.count.prompts++
		r.count.mu.Unlock()
	}
	return r.Registry.NodeSession(ctx, name, req)
}
func bindRetainedTestManager(t *testing.T, manager *harness.Manager, w *world, authority *delegateNodeAuthority) {
	t.Helper()
	manager.SetNodeSessionBinder(func(ctx context.Context, at harness.Placement, upstream, workdir string) (context.Context, error) {
		key, ok := execution.KeyOf(ctx)
		if !ok {
			return nil, errors.New("missing accepted scope")
		}
		record, err := w.attempts.Get(ctx, key.AttemptID)
		if err != nil {
			return nil, err
		}
		tracked, ok := w.tasks.Get(record.TaskID)
		if !ok {
			return nil, errors.New("missing task")
		}
		var epoch uint64
		for _, lease := range record.Leases {
			if lease.Key == "attempt:"+record.ID {
				epoch = lease.Epoch
			}
		}
		authority.mu.Lock()
		generation := authority.epoch
		authority.mu.Unlock()
		return harness.WithNodeSession(ctx, harness.NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "delegate-cluster", CoordinatorNodeID: fmt.Sprintf("hub-%d", generation), CoordinatorEpoch: generation, WriterGeneration: generation}, Binding: nodewire.SessionBinding{ProjectID: record.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: epoch, TaskEpoch: record.Execution.Epoch}, CommandID: record.TurnID}), nil
	})
}

func TestRetainedDelegateAcrossRealNodeTransportKeepsNativeQuestionAndInput(t *testing.T) {
	w, _ := executionWorld(t)
	executable := filepath.Join(t.TempDir(), "mockagent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", executable, "../../cmd/mockagent")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated agent: %v\n%s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	authority := &delegateNodeAuthority{epoch: 1}
	server := node.NewServer(node.ServerConfig{Name: "node-a", Token: "delegate-test-token", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]node.HarnessSpec{"mock": {Command: executable}}, SessionAuthorizer: authority, Listener: listener})
	serverCtx, stopServer := context.WithCancel(t.Context())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverCtx) }()
	t.Cleanup(func() {
		stopServer()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("node did not stop")
		}
	})
	count := &delegateWireCount{}
	transport := &delegateTransport{Registry: node.NewRegistry("delegate-cluster", map[string]node.Config{"node-a": {Addr: listener.Addr().String(), Token: "delegate-test-token"}}), count: count}
	t.Cleanup(transport.Close)
	first, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	first.SetTransports(transport)
	bindRetainedTestManager(t, first, w, authority)
	t.Cleanup(first.Stop)
	lifetime, cancel := context.WithCancel(t.Context())
	registry := execution.New(lifetime, w.tasks)
	w.service.SetExecution(registry)
	w.service.artifacts.SetExecution(registry)
	w.service.sessions = first
	w.service.SetGate(nil)
	t.Cleanup(func() {
		cancel()
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = registry.Shutdown(ctx)
	})
	asked := make(chan string, 1)
	w.service.SetRetainedQuestionHandlers(nil, func(ctx context.Context, binding QuestionBinding, q view.Question) (view.Answer, error) {
		native, session, id, ok := harness.NativeQuestionSource(ctx)
		if !ok || native.AttemptID != binding.Attempt || session != q.SessionID || id != q.RequestID {
			t.Error("native question source was not preserved")
		}
		asked <- q.RequestID
		<-ctx.Done()
		return view.Answer{}, ctx.Err()
	})
	parent := w.running(t, "codex")
	child, err := w.service.Start(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "askme", Agent: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	var originalQuestion string
	select {
	case originalQuestion = <-asked:
	case <-time.After(6 * time.Second):
		t.Fatal("native child never asked")
	}
	cancel()
	stopCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := registry.Shutdown(stopCtx); err != nil {
		t.Fatal(err)
	}
	first.Stop()
	transport.Close()
	stored, _ := w.tasks.Get(child.TaskID)
	if stored.State != task.StateRunning || stored.Result != nil || !stored.Attempts[0].Open() {
		t.Fatalf("original execution did not survive: %+v", stored)
	}
	authority.mu.Lock()
	authority.epoch = 2
	authority.mu.Unlock()
	nextTransport := &delegateTransport{Registry: node.NewRegistry("delegate-cluster", map[string]node.Config{"node-a": {Addr: listener.Addr().String(), Token: "delegate-test-token"}}), count: count}
	t.Cleanup(nextTransport.Close)
	next, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	next.SetTransports(nextTransport)
	bindRetainedTestManager(t, next, w, authority)
	t.Cleanup(next.Stop)
	service := recoveredDelegateService(t, w, next)
	service.SetRetainedQuestionHandlers(nil, func(ctx context.Context, binding QuestionBinding, q view.Question) (view.Answer, error) {
		native, session, id, ok := harness.NativeQuestionSource(ctx)
		if !ok || native.TaskID != child.TaskID || session != q.SessionID || id != originalQuestion || q.RequestID != originalQuestion || binding.ParentTask != parent.ID {
			t.Error("reattachment changed the original child question")
		}
		return view.Answer{Value: "Blue"}, nil
	})
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored = awaitDelegateResult(t, w.tasks, child.TaskID)
	if stored.State != task.StateDone || !strings.Contains(stored.Result.Answer, "accept:Blue") || len(stored.Attempts) != 1 {
		t.Fatalf("native child not resumed: %+v", stored)
	}
	records, err := w.attempts.ForTask(t.Context(), child.TaskID)
	if err != nil || len(records) != 1 || !strings.HasPrefix(records[0].Session, "ns_") || records[0].State != attempt.Bound {
		t.Fatalf("original receipt not committed: %+v %v", records, err)
	}
	var saved agentmcp.DelegateResult
	if err := json.Unmarshal(records[0].Result.Output, &saved); err != nil || saved.Answer != stored.Result.Answer {
		t.Fatalf("full native result missing: %+v %v", saved, err)
	}
	count.mu.Lock()
	defer count.mu.Unlock()
	if count.prompts != 1 {
		t.Fatalf("native input was replayed %d times", count.prompts)
	}
}
