//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/filedoc"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// orphaningShell starts the agent the way a harness can: through a shell
// that leaves a member of the agent's process group running on its own. The
// member's argument is unique to one test, so cleanup never signals a
// process the test did not start.
const orphaningShell = `echo $$ > "$2/leader"; sleep "$3" </dev/null >/dev/null 2>&1 & echo $! > "$2/member"; exec "$1"`

func orphaningNodeConfig(dir, agent, pause string) node.ServerConfig {
	return node.ServerConfig{Name: "worker", Token: "stop-test", StateDir: filepath.Join(dir, "state"), WorkspaceRoot: filepath.Join(dir, "workspace"),
		Harnesses:         map[string]node.HarnessSpec{"mock": {Command: "/bin/sh", Args: []string{"-c", orphaningShell, "orphaning", agent, dir, pause}}},
		SessionAuthorizer: &applicationStopAuthority{epoch: 1}}
}

// serveNode serves cfg until ctx ends and returns the address it listens on.
func serveNode(ctx context.Context, cfg node.ServerConfig) (string, <-chan error, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	cfg.Listener = listener
	served := make(chan error, 1)
	go func() { served <- node.NewServer(cfg).Serve(ctx) }()
	return listener.Addr().String(), served, nil
}

// TestApplicationStopKilledNodeSubprocess is the node a stop test kills: it
// serves native sessions until it is killed.
func TestApplicationStopKilledNodeSubprocess(t *testing.T) {
	dir := os.Getenv("STEVE_KILLED_NODE_DIR")
	if dir == "" {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	addr, _, err := serveNode(ctx, orphaningNodeConfig(dir, os.Getenv("STEVE_KILLED_NODE_AGENT"), os.Getenv("STEVE_KILLED_NODE_PAUSE")))
	if err != nil {
		t.Fatal(err)
	}
	if err := (&filedoc.Document{Path: filepath.Join(dir, "address")}).Save([]byte(addr)); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
}

// A node killed while a paused task's agent still ran leaves the agent's
// process group behind. Once the node is back, the stop the hub keeps
// retrying must end that group and settle the original execution.
func TestApplicationStopCompletesAfterTheNodeWasKilled(t *testing.T) {
	bounded, end := context.WithTimeout(t.Context(), 60*time.Second)
	defer end()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", agent, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated test agent: %v %s", err, output)
	}
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	t.Cleanup(func() {
		if pid := recordedPID(filepath.Join(dir, "member")); pid > 0 && cmdlineIs(pid, "sleep", pause) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if pid := recordedPID(filepath.Join(dir, "leader")); pid > 0 && cmdlineIs(pid, agent) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	killed := exec.Command(executable, "-test.run=^TestApplicationStopKilledNodeSubprocess$", "-test.timeout=90s")
	killed.Env = append(os.Environ(), "STEVE_KILLED_NODE_DIR="+dir, "STEVE_KILLED_NODE_AGENT="+agent, "STEVE_KILLED_NODE_PAUSE="+pause)
	log, err := os.Create(filepath.Join(dir, "node.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	killed.Stdout, killed.Stderr = log, log
	if err := killed.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = killed.Process.Kill()
		_ = killed.Wait()
	}()
	for deadline := time.Now().Add(15 * time.Second); recordedAddress(dir) == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("killed node never listened")
		}
	}

	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = book.Close() }()
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Goal: "original task", Channel: "console:stopping", Member: "mock", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "mock", "worker", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	record, err := attempts.Open(bounded, attempt.Spec{ID: "original-attempt", TaskID: tracked.ID, TurnID: "original-input", Kind: attempt.KindChat, Project: "p", Node: "worker", Harness: "mock", Agent: "mock", Execution: &token, Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "original-workspace", Project: "p", Node: "worker", Path: filepath.Join(dir, "workspace"), Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err = attempts.Advance(bounded, record.ID, attempt.Prepared, "test", nil); err != nil {
		t.Fatal(err)
	}
	binding := harness.NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "stop-cluster", CoordinatorNodeID: "coordinator", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: nodewire.SessionBinding{ProjectID: record.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(record), TaskEpoch: token.Epoch}, CommandID: attempt.InputCommandID(record)}
	connections := node.NewRegistry("stop-cluster", map[string]node.Config{"worker": {Addr: recordedAddress(dir), Token: "stop-test"}})
	first, _ := harness.NewManager(nil)
	first.SetTransports(connections)
	defer connections.Close()
	defer first.Stop()
	observer, detach := context.WithCancel(harness.WithNodeSession(bounded, binding))
	defer detach()
	runner, err := first.OpenSession(observer, harness.Placement{Node: "worker", Harness: "mock"}, "", record.Workspace.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if record, err = attempts.Advance(bounded, record.ID, attempt.Running, "test", func(r *attempt.Record) { r.Session = runner.ID() }); err != nil {
		t.Fatal(err)
	}
	asked, detached := make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, err := runner.(harness.TurnRunner).PromptTurn(observer, "askme", nil, nil, func(ctx context.Context, _ view.Question) (view.Answer, error) {
			close(asked)
			<-ctx.Done()
			return view.Answer{}, ctx.Err()
		}, nil)
		detached <- err
	}()
	select {
	case <-asked:
	case <-bounded.Done():
		t.Fatal("original task did not reach native question")
	}
	if _, err := tasks.SetAside(tracked.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	detach()
	if err := <-detached; !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("lost observer fabricated stop evidence: %v", err)
	}
	first.Stop()
	connections.Close()

	// The node dies with the paused task's agent still running.
	if err := killed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = killed.Wait()
	member := recordedPID(filepath.Join(dir, "member"))
	if !liveProcess(member) {
		t.Fatal("the group member ended with the node; the restart has nothing to stop")
	}
	nodeCtx, closeNode := context.WithCancel(t.Context())
	addr, served, err := serveNode(nodeCtx, orphaningNodeConfig(dir, agent, pause))
	if err != nil {
		closeNode()
		t.Fatal(err)
	}
	defer func() { closeNode(); <-served }()
	nextConnections := node.NewRegistry("stop-cluster", map[string]node.Config{"worker": {Addr: addr, Token: "stop-test"}})
	defer nextConnections.Close()
	next, _ := harness.NewManager(nil)
	next.SetTransports(nextConnections)
	next.SetStopRegistrar(execution.RegisterStopHandler)
	next.SetNodeSessionBinder(func(ctx context.Context, _ harness.Placement, _, _ string) (context.Context, error) {
		key, ok := execution.KeyOf(ctx)
		if !ok || key.AttemptID != record.ID || key.TaskID != record.TaskID || execution.Token(ctx) != nil {
			return nil, errors.New("stopping did not use the original read-only probe")
		}
		return harness.WithNodeSession(ctx, binding), nil
	})
	defer next.Stop()
	consumer := newApplicationStops(attempts, tasks, &applicationStopConnection{manager: next}, i18n.New(i18n.LocaleEN))
	if err := consumer.Reconcile(bounded); err != nil {
		t.Fatal(err)
	}
	stored, err := attempts.Get(bounded, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok, err := attempts.TaskStopReceipt(bounded, record.ID)
	if err != nil || !ok || receipt.Evidence.Session.ID != runner.ID() || stored.Unsettled || stored.State != attempt.Failed {
		t.Fatalf("the restarted node did not let the hub confirm the stop: state=%s unsettled=%v error=%q receipt=%v: %v", stored.State, stored.Unsettled, stored.Error, ok, err)
	}
	if liveProcess(member) {
		t.Fatal("the hub confirmed a stop while a member of the agent's process group still ran")
	}
}

func recordedAddress(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "address"))
	if err != nil {
		return ""
	}
	return string(raw)
}

func recordedPID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

func cmdlineIs(pid int, argv ...string) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.Equal(raw, []byte(strings.Join(argv, "\x00")+"\x00"))
}

// liveProcess is false for a pid that is gone or a zombie: neither runs.
func liveProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(raw, ')')
	fields := strings.Fields(string(raw[end+1:]))
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}
