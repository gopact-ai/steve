package delegate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// --- fakes -------------------------------------------------------------

type fakeNodes struct{ statuses []node.Status }

func (f fakeNodes) Statuses() []node.Status         { return f.statuses }
func (f fakeNodes) EnsureConnected(context.Context) {}

type fakeSessions struct {
	mu      sync.Mutex
	opened  []harness.Placement
	prompts []string
	servers [][]acp.MCPServer
	reply   func(prompt string) (string, error)
	// run, when set, is the child's whole turn: it sees the context and
	// may report progress, the way a real session does.
	run func(ctx context.Context, progress func(view.Progress)) (string, error)
}

func (f *fakeSessions) OpenSession(_ context.Context, at harness.Placement, _, _ string, servers []acp.MCPServer) (harness.Runner, error) {
	f.mu.Lock()
	f.opened = append(f.opened, at)
	f.servers = append(f.servers, servers)
	f.mu.Unlock()
	return &fakeRunner{owner: f}, nil
}

func (f *fakeSessions) CloseSession(context.Context, harness.Placement, string) error { return nil }

type fakeRunner struct{ owner *fakeSessions }

func (r *fakeRunner) ID() string { return "child-session" }
func (r *fakeRunner) Prompt(ctx context.Context, text string, progress func(view.Progress)) (string, []string, error) {
	r.owner.mu.Lock()
	r.owner.prompts = append(r.owner.prompts, text)
	reply, run := r.owner.reply, r.owner.run
	r.owner.mu.Unlock()
	if run != nil {
		out, err := run(ctx, progress)
		return out, nil, err
	}
	if reply != nil {
		out, err := reply(text)
		return out, nil, err
	}
	return "done\nREF: git deadbeef — the result", nil, nil
}
func (r *fakeRunner) Cancel(context.Context) error { return nil }
func (r *fakeRunner) Abort()                       {}

// fakeEndpoints answers with a node-local loopback, as the registry would.
type fakeEndpoints struct{}

func (fakeEndpoints) MCPEndpoint(_ context.Context, nodeName string) (string, error) {
	return "http://127.0.0.1:4545/mcp#" + nodeName, nil
}

// --- fixture -----------------------------------------------------------

type world struct {
	tasks    *task.Store
	sessions *fakeSessions
	gate     *agentmcp.Server
	service  *Service
}

func newWorld(t *testing.T) *world {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex":   {Harness: "mock", Default: true},
		"builder": {Harness: "mock", Node: "node-a"},
		"shipper": {Harness: "mock", Node: "node-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := roster.New(catalog)
	r.SetHubCapabilities([]string{"basic"})
	r.SetNodes(fakeNodes{statuses: []node.Status{
		{Name: "node-a", Up: true, Advert: nodewire.Advert{Node: "node-a", Capabilities: []string{"gpu"},
			Harnesses: []nodewire.Harness{{ID: "mock"}}}},
		{Name: "node-b", Up: true, Advert: nodewire.Advert{Node: "node-b", Capabilities: []string{"prod-cred"},
			Harnesses: []nodewire.Harness{{ID: "mock"}}}},
	}})

	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	tasks.SetBudget(10, time.Hour)

	gate, err := agentmcp.New(0)
	if err != nil {
		t.Fatal(err)
	}
	sessions := &fakeSessions{}
	art, att := stores(t)
	service := New(tasks, r, sessions, capability.NewAssembler(nil), art, "hub")
	service.SetLedger(att, art)
	service.SetGate(gate)
	service.SetEndpoints(fakeEndpoints{})
	gate.SetDelegator(service)
	return &world{tasks: tasks, sessions: sessions, gate: gate, service: service}
}

// running opens a task for the caller, the way a chat turn would.
func (w *world) running(t *testing.T, member string) task.Task {
	t.Helper()
	parent, err := w.tasks.Create(task.Task{Goal: "the big goal", Channel: "chat", Member: member, Node: "hub", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Begin(parent.ID, member, "hub", ""); err != nil {
		t.Fatal(err)
	}
	return parent
}

// --- tests -------------------------------------------------------------

func TestDelegateOpensAChildOnTheRightMachine(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")

	result, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{
		Goal: "train the model", Requires: []string{"gpu"},
		Refs: []string{"git abc123", "not a ref at all"}, Expect: "a checkpoint file",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Agent != "builder" || result.Node != "node-a" {
		t.Fatalf("placed on %s@%s, want builder@node-a", result.Agent, result.Node)
	}
	if result.Outcome != "ok" || len(result.Refs) != 1 || result.Refs[0] != "git deadbeef" {
		t.Fatalf("result = %+v", result)
	}

	// A real child task exists under the parent, funded from it, and done.
	child, ok := w.tasks.Get(result.TaskID)
	if !ok || child.Parent != parent.ID {
		t.Fatalf("child task = %+v", child)
	}
	if child.State != task.StateDone {
		t.Fatalf("child state = %s", child.State)
	}
	if child.Budget.MaxTurns >= 10 {
		t.Fatalf("child ceiling %d was not carved from the parent's remainder", child.Budget.MaxTurns)
	}
	charged, _ := w.tasks.Get(parent.ID)
	if charged.Budget.Turns < 2 {
		t.Fatalf("parent turns = %d; the child's spend was not charged back", charged.Budget.Turns)
	}

	// The session was opened where the agent lives.
	if len(w.sessions.opened) != 1 || w.sessions.opened[0].Node != "node-a" {
		t.Fatalf("session opened at %v", w.sessions.opened)
	}
	// And the child got its own messaging token, pointed at a loopback on
	// its own machine rather than at the hub's.
	if len(w.sessions.servers[0]) != 1 {
		t.Fatalf("child got %d MCP servers, want its own messaging server", len(w.sessions.servers[0]))
	}
	server := w.sessions.servers[0][0]
	if !strings.Contains(server.URL, "node-a") {
		t.Fatalf("child messaging URL = %q, want the node's own loopback", server.URL)
	}
	token := ""
	for _, h := range server.Headers {
		token = strings.TrimPrefix(h.Value, "Bearer ")
	}
	if token == "" {
		t.Fatal("child got no bearer token")
	}
}

// What the child sees is assembled by the hub, typed and bounded — and the
// parent's transcript is not in it.
func TestChildSeesContextNotTranscript(t *testing.T) {
	w := newWorld(t)
	w.running(t, "codex")
	_, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{
		Goal: "ship it", Requires: []string{"prod-cred"}, Refs: []string{"git feature/x"},
		Expect: "deployed and healthy",
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt := w.sessions.prompts[0]
	for _, want := range []string{
		"ship it",        // the goal
		"the big goal",   // ancestry: what this serves
		"git: feature/x", // the ref, as a pointer
		"完成的标准：deployed", // the acceptance line
		"prod-cred",      // facts about the machine it landed on
		"先定向再动手",         // bearings
		"REF:",           // the reporting contract
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("child prompt is missing %q", want)
		}
	}
	if strings.Contains(prompt, "transcript") {
		t.Error("the child was handed something called a transcript")
	}
}

func TestCannotDelegateWithoutARunningTask(t *testing.T) {
	w := newWorld(t)
	_, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "x", Requires: []string{"gpu"}})
	if err == nil || !strings.Contains(err.Error(), "no running task") {
		t.Fatalf("err = %v", err)
	}
}

func TestCannotDelegateToSelf(t *testing.T) {
	w := newWorld(t)
	w.running(t, "codex")
	_, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "x", Agent: "codex"})
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("err = %v", err)
	}
	// Placement by capability also never picks the caller.
	w2 := newWorld(t)
	w2.running(t, "builder")
	res, err := w2.service.Delegate(t.Context(), "chat", "builder", agentmcp.DelegateRequest{Goal: "x", Requires: []string{"any"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent == "builder" {
		t.Fatal("capability placement handed the work back to the caller")
	}
}

// A→B→A is refused by the tree, and the refusal reaches the caller as a
// tool error it can act on.
func TestCycleIsRefusedAtTheTool(t *testing.T) {
	w := newWorld(t)
	w.running(t, "codex")
	// The child, when it runs, tries to delegate back up to codex.
	w.sessions.reply = func(prompt string) (string, error) {
		_, err := w.service.Delegate(context.Background(), "chat", "builder", agentmcp.DelegateRequest{
			Goal: "hand it back", Agent: "codex",
		})
		if err == nil {
			return "", errors.New("the cycle was accepted")
		}
		if !strings.Contains(err.Error(), "loop") {
			return "", errors.New("wrong refusal: " + err.Error())
		}
		return "refused correctly: " + err.Error(), nil
	}
	res, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "go", Requires: []string{"gpu"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Answer, "refused correctly") {
		t.Fatalf("child answer = %q", res.Answer)
	}
}

// A failed child comes back as an error the parent can handle, and the
// spend is still charged.
func TestChildFailureIsAResultNotAHang(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	w.sessions.reply = func(string) (string, error) { return "", errors.New("node-a blew up") }
	res, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "go", Requires: []string{"gpu"}})
	if err == nil || !strings.Contains(err.Error(), "blew up") {
		t.Fatalf("err = %v", err)
	}
	if res.Outcome != "error" {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	child, _ := w.tasks.Get(res.TaskID)
	if child.State != task.StateFailed {
		t.Fatalf("child state = %s", child.State)
	}
	charged, _ := w.tasks.Get(parent.ID)
	if charged.Budget.Turns < 2 {
		t.Fatal("a failed child's spend was not charged to the parent")
	}
}

// A spent parent cannot delegate: the tree is the brake.
func TestExhaustedParentIsRefused(t *testing.T) {
	w := newWorld(t)
	w.tasks.SetBudget(1, time.Hour)
	w.running(t, "codex") // Begin consumed the one turn
	_, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "go", Requires: []string{"gpu"}})
	if err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("err = %v", err)
	}
	if len(w.sessions.opened) != 0 {
		t.Fatal("a session was opened for a delegation the budget refused")
	}
}

var _ = permission.New

// The tool form: start returns quickly with a running child, await returns
// the result, and a caller cannot await someone else's child.
func TestStartAndAwaitOutliveTheRequest(t *testing.T) {
	w := newWorld(t)
	w.running(t, "codex")
	w.service.InlineWait = 10 * time.Millisecond
	observed := make(chan Child, 8)
	w.service.SetObserver(func(c Child, _ view.Progress) { observed <- c })
	release := make(chan struct{})
	w.sessions.reply = func(string) (string, error) {
		<-release
		return "done late\nREF: git late1", nil
	}

	// The request that starts the child is cancelled straight away — the
	// way an MCP client timing out looks from here.
	reqCtx, cancelReq := context.WithCancel(t.Context())
	started, err := w.service.Start(reqCtx, "chat", "codex", agentmcp.DelegateRequest{Goal: "slow thing", Requires: []string{"gpu"}})
	select {
	case c := <-observed:
		if c.Task != started.TaskID || c.Conversation != "chat" || c.State != "running" || c.Goal != "slow thing" {
			t.Fatalf("initial child snapshot = %+v", c)
		}
	default:
		t.Fatal("Start returned before registering the child with its parent reply")
	}
	cancelReq()
	if err != nil {
		t.Fatal(err)
	}
	if started.State != "running" || started.TaskID == "" || started.Node != "node-a" {
		t.Fatalf("start = %+v, want a running child on node-a", started)
	}

	// Nobody else may await it.
	other, _ := w.tasks.Create(task.Task{Goal: "other", Channel: "chat", Member: "shipper", ProjectID: "p"})
	_, _ = w.tasks.Begin(other.ID, "shipper", "hub", "")
	if _, err := w.service.Await(t.Context(), "chat", "shipper", agentmcp.AwaitRequest{TaskID: started.TaskID}); err == nil {
		t.Fatal("a stranger awaited someone else's delegation")
	}

	// The caller awaits; bounded; still running.
	interim, err := w.service.Await(t.Context(), "chat", "codex", agentmcp.AwaitRequest{TaskID: started.TaskID, WaitSeconds: 1})
	if err != nil || interim.State != "running" {
		t.Fatalf("interim = %+v, %v", interim, err)
	}

	close(release)
	final, err := w.service.Await(t.Context(), "chat", "codex", agentmcp.AwaitRequest{TaskID: started.TaskID, WaitSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "done" || final.Outcome != "ok" || len(final.Refs) != 1 || final.Refs[0] != "git late1" {
		t.Fatalf("final = %+v", final)
	}
	stored, _ := w.tasks.Get(started.TaskID)
	if stored.State != task.StateDone {
		t.Fatalf("child task state = %s; the cancelled request must not have taken the child down", stored.State)
	}
}

// stores gives a test a project "p" on the hub, attempts, and artifacts
// whose node side runs on this machine.
func stores(t *testing.T) (*artifact.Store, *attempt.Service) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(context.Background(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	return artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()}), attempt.New(book)
}

// A child is cut for silence, not for taking long: one that keeps
// reporting past the limit finishes, one that goes quiet is cancelled.
func TestAChildIsCutForSilenceNotForWork(t *testing.T) {
	w := newWorld(t)
	w.running(t, "codex")
	// The idle clock includes ledger/session setup. Leave enough room for
	// that work under the race detector on shared CI runners, while the
	// active child still runs for longer than one whole silence window.
	w.service.MaxSilence = time.Second

	w.sessions.run = func(ctx context.Context, progress func(view.Progress)) (string, error) {
		for i := 0; i < 40; i++ { // 2s of work, never 1s of silence
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			progress(view.Progress{})
		}
		return "done\nREF: git chatty", nil
	}
	res, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "build it", Requires: []string{"gpu"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "done" {
		t.Fatalf("a working child was cut: %+v", res)
	}

	w.sessions.run = func(ctx context.Context, _ func(view.Progress)) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(4 * time.Second):
			return "too late", nil
		}
	}
	res, err = w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "hang", Requires: []string{"gpu"}})
	if err == nil || res.State != "failed" {
		t.Fatalf("a silent child was not cut: res=%+v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("err = %v, want the idle deadline", err)
	}
}

// The child's answer is the result whether or not its files landed.
func TestAChildsAnswerSurvivesLanding(t *testing.T) {
	w := newWorld(t)
	w.running(t, "codex")
	w.sessions.reply = func(string) (string, error) { return "I wrote it.\nREF: git abc123 — the change", nil }
	res, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "write it", Requires: []string{"gpu"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "done" || !strings.Contains(res.Answer, "I wrote it.") {
		t.Fatalf("answer lost: %+v", res)
	}
	if !strings.Contains(strings.Join(res.Refs, "\n"), "git abc123") {
		t.Fatalf("refs lost: %+v", res.Refs)
	}
}
