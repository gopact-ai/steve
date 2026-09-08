package node

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

func TestNodeSessionExplicitTaskStopEndsNativePromptWhileLifetimeOnlyDetaches(t *testing.T) {
	for _, mode := range []string{"paused", "cancelled", "lifetime"} {
		t.Run(mode, func(t *testing.T) {
			authority := &sessionAuthorityTest{epoch: 1, writer: 1}
			server := startNode(t, ServerConfig{Name: "worker", Token: "explicit-stop", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
			registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "explicit-stop"}})
			defer registry.Close()
			manager, _ := harness.NewManager(nil)
			manager.SetTransports(registry)
			manager.SetStopRegistrar(execution.RegisterStopHandler)
			defer manager.Stop()
			lifetime, cancelLifetime := context.WithCancel(t.Context())
			defer cancelLifetime()
			tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
			if err != nil {
				t.Fatal(err)
			}
			tracked, err := tasks.Create(task.Task{Channel: "console:stop", Member: "mock"})
			if err != nil {
				t.Fatal(err)
			}
			executions := execution.New(lifetime, tasks)
			scope, err := executions.Begin(lifetime, execution.Key{TaskID: tracked.ID, AttemptID: "attempt-1"})
			if err != nil {
				t.Fatal(err)
			}
			request := nodeSessionRequest("open")
			request.Binding.TaskID = tracked.ID
			binding := harness.NodeSessionContext{Authority: request.Authority, Binding: request.Binding, CommandID: "explicit-stop"}
			ctx := harness.WithNodeSession(scope.Context(), binding)
			runner, err := manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			asked, result := make(chan struct{}), make(chan error, 1)
			go func() {
				_, _, runErr := runner.(harness.TurnRunner).PromptTurn(ctx, "askme", nil, nil, func(ctx context.Context, _ view.Question) (view.Answer, error) {
					close(asked)
					<-ctx.Done()
					return view.Answer{}, ctx.Err()
				}, nil)
				var unresolved error
				if errors.Is(runErr, harness.ErrStopUnconfirmed) {
					unresolved = &execution.RetainedObserverDetached{AttemptID: "attempt-1", NodeID: "worker", SessionID: runner.ID(), Cause: runErr}
				}
				scope.Finish(unresolved)
				result <- runErr
			}()
			select {
			case <-asked:
			case <-time.After(5 * time.Second):
				t.Fatal("node did not produce a pending question")
			}
			bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if mode == "lifetime" {
				cancelLifetime()
				if err := executions.Shutdown(bounded); err != nil {
					t.Fatal(err)
				}
			} else {
				ids, err := tasks.SetAside(tracked.ID, task.State(mode))
				if err != nil {
					t.Fatal(err)
				}
				if err := executions.Stop(ids, task.ErrExecutionStopped).Wait(bounded); err != nil {
					t.Fatalf("explicit stop left original execution running: %v", err)
				}
			}
			runErr := <-result
			inspectCtx := harness.WithNodeSession(bounded, binding)
			state, err := runner.(harness.RetainedSessionInspector).InspectRetained(inspectCtx)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "lifetime" {
				if !errors.Is(runErr, harness.ErrStopUnconfirmed) || state.Command == nil || state.Command.Settled || state.ProcessStopped || len(state.Questions) != 1 || state.Questions[0].State != "pending" {
					t.Fatalf("coordinator lifetime stopped native work: err=%v state=%+v", runErr, state)
				}
				attached, err := manager.AttachRetainedSession(inspectCtx, harness.Placement{Node: "worker", Harness: "mock"}, runner.ID(), "")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := attached.ResumeTurn(inspectCtx, nil, func(context.Context, view.Question) (view.Answer, error) { return view.Answer{Value: "Blue"}, nil }, nil); err != nil {
					t.Fatalf("detached native command could not continue: %v", err)
				}
			} else if !acphost.PromptSettled(runErr) || !errors.Is(runErr, harness.ErrTurnCanceled) || state.Command == nil || !state.Command.Settled || !state.Command.CancelRequested {
				t.Fatalf("task stop lacks exact original native receipt: err=%v state=%+v", runErr, state)
			}
		})
	}
}

func TestNodeSessionRetainedAttachmentCanStopTheOriginalNativeCommand(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	server := startNode(t, ServerConfig{Name: "worker", Token: "attached-stop", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "attached-stop"}})
	defer registry.Close()
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Channel: "console:attached", Member: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	request := nodeSessionRequest("open")
	request.Binding.TaskID = tracked.ID
	request.CommandID, request.Harness, request.Workdir = "original-open", "mock", t.TempDir()
	state, err := registry.NodeSession(t.Context(), "worker", request)
	if err != nil {
		t.Fatal(err)
	}
	request.ID, request.CommandID, request.Action = state.ID, "original-input", "prompt"
	request.InputSequence, request.Text = 1, "askme"
	if _, err := registry.NodeSession(t.Context(), "worker", request); err != nil {
		t.Fatal(err)
	}
	authority.mu.Lock()
	authority.epoch, authority.writer = 2, 2
	authority.mu.Unlock()
	request.Authority.CoordinatorNodeID, request.Authority.CoordinatorEpoch, request.Authority.WriterGeneration = "hub-b", 2, 2
	manager, _ := harness.NewManager(nil)
	manager.SetTransports(registry)
	manager.SetStopRegistrar(execution.RegisterStopHandler)
	defer manager.Stop()
	executions := execution.New(t.Context(), tasks)
	scope, err := executions.Begin(t.Context(), execution.Key{TaskID: tracked.ID, AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	binding := harness.NodeSessionContext{Authority: request.Authority, Binding: request.Binding, CommandID: request.CommandID}
	ctx := harness.WithNodeSession(scope.Context(), binding)
	runner, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, request.ID, request.Workdir)
	if err != nil {
		t.Fatal(err)
	}
	asked, result := make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, runErr := runner.ResumeTurn(ctx, nil, func(ctx context.Context, _ view.Question) (view.Answer, error) {
			close(asked)
			<-ctx.Done()
			return view.Answer{}, ctx.Err()
		}, nil)
		var unresolved error
		if !acphost.PromptSettled(runErr) {
			unresolved = runErr
		}
		scope.Finish(unresolved)
		result <- runErr
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("original native question did not reach replacement observer")
	}
	ids, err := tasks.SetAside(tracked.ID, task.StatePaused)
	if err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := executions.Stop(ids, task.ErrExecutionStopped).Wait(bounded); err != nil {
		t.Fatalf("reattached scope did not stop original node command: %v", err)
	}
	if err := <-result; !errors.Is(err, harness.ErrTurnCanceled) {
		t.Fatalf("reattached native stop result: %v", err)
	}
	request.Action = "attach"
	state, err = registry.NodeSession(bounded, "worker", request)
	if err != nil || state.ID != request.ID || state.InputAccepted != 1 || state.Command == nil || state.Command.ID != "original-input" || !state.Command.Settled || !state.Command.CancelRequested {
		t.Fatalf("reattached stop changed original native input: %+v %v", state, err)
	}
}

type delayedStopPromptAuthority struct {
	base             sessionAuthorityTest
	checked, release chan struct{}
	once             sync.Once
}

func (a *delayedStopPromptAuthority) AuthorizeNodeSession(ctx context.Context, principal string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, action nodewire.SessionAction) error {
	if err := a.base.AuthorizeNodeSession(ctx, principal, authority, binding, action); err != nil {
		return err
	}
	if action == "prompt" {
		a.once.Do(func() { close(a.checked) })
		<-a.release
	}
	return nil
}

func TestNodeSessionStopFencesAnAlreadyAuthorizedButUnacceptedPrompt(t *testing.T) {
	authority := &delayedStopPromptAuthority{base: sessionAuthorityTest{epoch: 1, writer: 1}, checked: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(authority.release) }) }
	defer release()
	server := startNode(t, ServerConfig{Name: "worker", Token: "delayed-prompt", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "delayed-prompt"}})
	defer registry.Close()
	manager, _ := harness.NewManager(nil)
	manager.SetTransports(registry)
	manager.SetStopRegistrar(execution.RegisterStopHandler)
	defer manager.Stop()
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Channel: "console:delayed", Member: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	executions := execution.New(t.Context(), tasks)
	scope, err := executions.Begin(t.Context(), execution.Key{TaskID: tracked.ID, AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	request := nodeSessionRequest("open")
	request.Binding.TaskID = tracked.ID
	binding := harness.NodeSessionContext{Authority: request.Authority, Binding: request.Binding, CommandID: "delayed-input"}
	ctx := harness.WithNodeSession(scope.Context(), binding)
	runner, err := manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, _, runErr := runner.Prompt(ctx, "askme", nil)
		var unresolved error
		if !acphost.PromptSettled(runErr) {
			unresolved = runErr
		}
		scope.Finish(unresolved)
		result <- runErr
	}()
	select {
	case <-authority.checked:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt never reached node authorization")
	}
	ids, err := tasks.SetAside(tracked.ID, task.StatePaused)
	if err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	waiting := executions.Stop(ids, task.ErrExecutionStopped)
	if err := waiting.Wait(bounded); err != nil {
		t.Fatalf("native stop did not finish before delayed prompt: %v", err)
	}
	state, err := runner.(harness.RetainedSessionInspector).InspectRetained(harness.WithNodeSession(bounded, binding))
	if err != nil {
		t.Fatal(err)
	}
	if !state.ProcessStopped || state.State != "closed" {
		t.Fatalf("idle receipt left already authorized prompt able to dispatch: %+v", state)
	}
	release()
	if err := <-result; !errors.Is(err, harness.ErrTurnCanceled) {
		t.Fatalf("late prompt stop result: %v", err)
	}
	if state.InputAccepted != 0 || state.Command != nil {
		t.Fatalf("stop dispatched the original input: %+v", state)
	}
}
