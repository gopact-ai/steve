package turn

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

// waitDeadline bounds how long a test waits for a goroutine to reach a known
// point. A passing test never spends it; a generous bound is what keeps the
// suite usable under -race, where everything runs several times slower.
const waitDeadline = 10 * time.Second

type fakeManager struct {
	runners  map[string]*fakeRunner
	opened   []string
	workdirs []string
	servers  [][]acp.MCPServer
	fail     error
	failOnce bool
	mcpHTTP  bool
}

func (m *fakeManager) OpenSession(_ context.Context, harnessID, upstreamID, workdir string, servers []acp.MCPServer) (harness.Runner, error) {
	if m.fail != nil {
		err := m.fail
		if m.failOnce {
			m.fail = nil
		}
		return nil, err
	}
	id := upstreamID
	if id == "" {
		id = harnessID + "-session"
	}
	runner := m.runners[harnessID]
	runner.id = id
	m.opened = append(m.opened, harnessID+":"+upstreamID)
	m.workdirs = append(m.workdirs, workdir)
	m.servers = append(m.servers, servers)
	return runner, nil
}

func (m *fakeManager) CloseSession(context.Context, string, string) error { return nil }

func (m *fakeManager) SupportsHTTPMCP(context.Context, string) (bool, error) { return m.mcpHTTP, nil }

type fakeRunner struct {
	id       string
	prompts  []string
	reply    string
	activity []string
	started  chan struct{}
	done     chan struct{}
	err      error
	canceled atomic.Bool
	cancels  atomic.Int32
	aborts   atomic.Int32
	stop     sync.Once
	// start fires once per runner; a turn can now be interrupted by the
	// next one, so Prompt is reached more than once with the same runner.
	start sync.Once
	// mu guards prompts, which two turns can append to at the same moment
	// while one is being interrupted by the other.
	mu sync.Mutex
}

func (r *fakeRunner) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

func (r *fakeRunner) ID() string { return r.id }
func (r *fakeRunner) Prompt(ctx context.Context, prompt string, _ func(view.Progress)) (string, []string, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
	if r.started != nil {
		r.start.Do(func() { close(r.started) })
		select {
		case <-r.done:
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
	if r.canceled.Load() {
		return "", nil, context.Canceled
	}
	if r.reply != "" {
		return r.reply, append([]string{}, r.activity...), r.err
	}
	return "reply: " + prompt, nil, r.err
}
func (r *fakeRunner) Cancel(context.Context) error {
	r.cancels.Add(1)
	r.canceled.Store(true)
	if r.done != nil {
		r.stop.Do(func() { close(r.done) })
	}
	return nil
}
func (r *fakeRunner) Abort() { r.aborts.Add(1) }

func TestCoordinatorDropsSessionOnTurnErrorWithoutKillingProcess(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Workspace: t.TempDir(), SystemPrompt: "rules", Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{err: errors.New("prompt failed")}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err == nil {
		t.Fatal("expected prompt error")
	}
	// The turn ended with an error while the context was intact: the process
	// is healthy and must not be canceled or killed.
	if runner.cancels.Load() != 0 || runner.aborts.Load() != 0 {
		t.Fatalf("healthy process was torn down: cancels=%d aborts=%d", runner.cancels.Load(), runner.aborts.Load())
	}
	if _, ok := store.Conversation("chat").Sessions["codex"]; ok {
		t.Fatal("failed turn session was retained")
	}
}

func TestCoordinatorAbortsStuckTurnOnTimeout(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, 50*time.Millisecond)
	if _, err := handle(coordinator, t.Context(), "stuck"); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if runner.aborts.Load() == 0 {
		t.Fatal("stuck turn did not abort the process")
	}
	if _, ok := store.Conversation("chat").Sessions["codex"]; ok {
		t.Fatal("timed-out session was retained")
	}
}

func TestCoordinatorDropsStaleSessionWhenOpenFails(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	assembler := capability.NewAssembler(nil)
	capabilities, err := assembler.Assemble(catalog.Default())
	if err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err := store.SaveSession(state.Session{
		ConversationID: "chat", AgentID: "codex", HarnessID: "codex",
		UpstreamID: "stale-session", Workspace: catalog.Default().Workspace,
		CapabilityHash: capabilities.Fingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	failing := &fakeManager{fail: errors.New("session/load: not found")}
	coordinator := New(catalog, store, assembler, failing, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err == nil {
		t.Fatal("expected open error")
	}
	if _, ok := store.Conversation("chat").Sessions["codex"]; ok {
		t.Fatal("unreopenable session record was retained")
	}
}

func TestCoordinatorRetriesNewSessionAfterLoadFails(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	assembler := capability.NewAssembler(nil)
	capabilities, err := assembler.Assemble(catalog.Default())
	if err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err := store.SaveSession(state.Session{
		ConversationID: "chat", AgentID: "codex", HarnessID: "codex",
		UpstreamID: "stale-session", Workspace: catalog.Default().Workspace,
		CapabilityHash: capabilities.Fingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{
		fail:     errors.New("session/load: authentication required"),
		failOnce: true,
		runners:  map[string]*fakeRunner{"codex": {reply: "recovered"}},
	}
	coordinator := New(catalog, store, assembler, manager, time.Minute)
	result, err := handle(coordinator, t.Context(), "hello")
	if err != nil || result.Text != "recovered" {
		t.Fatalf("retry = %#v, %v", result, err)
	}
	if len(manager.opened) != 1 || manager.opened[0] != "codex:" {
		t.Fatalf("opens = %v", manager.opened)
	}
}

func TestCoordinatorStatusHasFields(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	coordinator := New(catalog, store, capability.NewAssembler(nil), &fakeManager{}, time.Minute)
	result, err := handle(coordinator, t.Context(), "/status")
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "状态" || len(result.Fields) < 3 || !result.Fields[0].IsMetric || result.Fields[2].Wide != true {
		t.Fatalf("fields = %#v title=%q", result.Fields, result.Title)
	}
	if !strings.Contains(result.Text, "Agent") {
		t.Fatalf("text fallback missing: %q", result.Text)
	}
}

func TestCoordinatorCancelDuringOpenCancelsContextImmediately(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &blockingOpenManager{started: make(chan struct{})}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	turnDone := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "hello")
		turnDone <- err
	}()
	select {
	case <-manager.started:
	case <-time.After(waitDeadline):
		t.Fatal("open did not start")
	}
	started := time.Now()
	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil || !strings.Contains(result.Text, i18n.New(i18n.LocaleZH).T(i18n.CancelRequested, "codex")) {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("cancel waited %s for a turn that had no runner yet", time.Since(started))
	}
	select {
	case err := <-turnDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled open, got %v", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("turn did not finish after cancel")
	}
}

type blockingOpenManager struct {
	started chan struct{}
	once    sync.Once
}

func (m *blockingOpenManager) OpenSession(ctx context.Context, _, _, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingOpenManager) CloseSession(context.Context, string, string) error { return nil }

func (m *blockingOpenManager) SupportsHTTPMCP(context.Context, string) (bool, error) {
	return false, nil
}

func TestCoordinatorPendingCancelStopsNextTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if result, err := handle(coordinator, t.Context(), "/cancel"); err != nil || result.Text != i18n.New(i18n.LocaleZH).T(i18n.NoRunningTurn) {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
	if _, err := handle(coordinator, t.Context(), "hello"); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected pending cancel to stop the next turn, got %v", err)
	}
	if len(runner.seen()) != 0 {
		t.Fatalf("canceled turn still prompted the agent: %v", runner.seen())
	}
}

func TestCoordinatorRejectsTaintedSession(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Workspace: t.TempDir(), Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err := store.SaveSession(state.Session{
		ConversationID: "chat", AgentID: "codex", HarnessID: "codex", Workspace: catalog.Default().Workspace,
		CapabilityHash: "ignored", Tainted: true,
	}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err == nil || !strings.Contains(err.Error(), "/new") {
		t.Fatalf("expected tainted session error, got %v", err)
	}
}

func TestCoordinatorSwitchesAgentsAndRestoresSessions(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex":  {Harness: "codex", Default: true},
		"claude": {Harness: "claude", SystemPrompt: "Act as Claude."},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}, "claude": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	first, err := handle(coordinator, t.Context(), "hello")
	if err != nil || first.AgentID != "codex" {
		t.Fatalf("default turn = %#v, %v", first, err)
	}
	second, err := handle(coordinator, t.Context(), "@claude fix it")
	if err != nil || second.AgentID != "claude" {
		t.Fatalf("tag turn = %#v, %v", second, err)
	}
	if got := manager.runners["claude"].prompts[0]; !strings.Contains(got, "Act as Claude.") || !strings.Contains(got, "fix it") {
		t.Fatalf("unexpected Claude prompt: %q", got)
	}
	if _, err := handle(coordinator, t.Context(), "@codex again"); err != nil {
		t.Fatal(err)
	}
	if got := manager.opened[len(manager.opened)-1]; got != "codex:codex-session" {
		t.Fatalf("Codex session was not restored: %q", got)
	}
	if _, err := handle(coordinator, t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	if got := manager.runners["codex"].prompts[2]; strings.Contains(got, "Act as") {
		t.Fatalf("instructions were injected twice: %q", got)
	}
}

func TestCoordinatorRejectsCapabilityDrift(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	conversation := store.Conversation("chat")
	session := conversation.Sessions["codex"]
	session.CapabilityHash = "stale"
	if err := store.DeleteSession("chat", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "again"); err == nil {
		t.Fatal("expected capability drift error")
	}
}

func TestCoordinatorCancelsRunningTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	turnDone := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "long task")
		turnDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(waitDeadline):
		t.Fatal("turn did not start")
	}
	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil || !strings.Contains(result.Text, i18n.New(i18n.LocaleZH).T(i18n.CancelRequested, "codex")) {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
	select {
	case err := <-turnDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("turn did not finish after cancel")
	}
	if _, ok := store.Conversation("chat").Sessions["codex"]; ok {
		t.Fatal("prompt goroutine did not clean up the canceled session")
	}
}

func TestCoordinatorCancelWithoutRunningTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil || result.Text != i18n.New(i18n.LocaleZH).T(i18n.NoRunningTurn) {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
}

func TestCoordinatorKeepsSessionWhenAgentCancelsTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{err: fmt.Errorf("%w: %w", harness.ErrTurnCanceled, context.Canceled)}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err == nil {
		t.Fatal("expected canceled turn error")
	}
	if runner.aborts.Load() != 0 {
		t.Fatal("graceful agent cancel must not abort the process")
	}
	if runner.cancels.Load() != 0 {
		t.Fatal("graceful agent cancel must not re-send cancel")
	}
	session, ok := store.Conversation("chat").Sessions["codex"]
	if !ok {
		t.Fatal("gracefully canceled session was deleted")
	}
	if session.Tainted {
		t.Fatal("canceled session left tainted")
	}
	if !session.InstructionsApplied {
		t.Fatal("canceled session did not record instruction state")
	}
}

// An agent that ends a cancelled turn itself leaves the session consistent,
// so an interrupt must not throw it away — otherwise steering would cost the
// agent its entire context every time the user redirects it.
func TestGracefullyCancelledTurnKeepsItsSession(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{id: "sess-1", err: harness.ErrTurnCanceled}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "first"); !errors.Is(err, harness.ErrTurnCanceled) {
		t.Fatalf("err = %v, want ErrTurnCanceled", err)
	}
	saved := store.Conversation("chat").Sessions["codex"]
	if saved.UpstreamID == "" {
		t.Fatalf("session dropped after a settled cancel: %+v", saved)
	}
	if saved.Tainted {
		t.Fatal("session left tainted after a settled cancel")
	}
}

// A turn the agent never settled leaves the session mid-flight, so it is
// dropped instead. The two paths are what make graceful cancellation worth
// doing at all.
func TestAbandonedTurnDropsItsSession(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{id: "sess-1", err: context.Canceled}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "first"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if saved := store.Conversation("chat").Sessions["codex"]; saved.UpstreamID != "" {
		t.Fatalf("abandoned turn kept its session: %+v", saved)
	}
}

func TestCoordinatorSwitchOnlyAndNew(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true}, "claude": {Harness: "claude"},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}, "claude": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	result, err := handle(coordinator, t.Context(), "/use claude")
	if err != nil || result.Text != i18n.New(i18n.LocaleZH).T(i18n.Switched, "claude") {
		t.Fatalf("switch = %#v, %v", result, err)
	}
	if _, err := handle(coordinator, t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "/new"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Conversation("chat").Sessions["claude"]; ok {
		t.Fatal("/new did not delete active agent session")
	}
}

func TestCoordinatorEnglishLocale(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true}, "claude": {Harness: "claude"},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}, "claude": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	coordinator.SetCatalog(i18n.New(i18n.LocaleEN))
	result, err := handle(coordinator, t.Context(), "/use claude")
	if err != nil || result.Text != i18n.New(i18n.LocaleEN).T(i18n.Switched, "claude") {
		t.Fatalf("en switch = %#v, %v", result, err)
	}
}

func handle(c *Coordinator, ctx context.Context, input string) (Result, error) {
	return c.Handle(ctx, Request{ConversationID: "chat", Input: input})
}

// "+ 还有件事" waits for the running turn; a bare message replaces it. The
// prefix is what keeps follow-up and correction from being the same gesture.
func TestPlusPrefixQueuesInsteadOfInterrupting(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "long running")
		first <- err
	}()
	<-runner.started

	second := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "+ 接着做第二件")
		second <- err
	}()

	// The queued prompt must not touch the running turn: no cancel, no
	// second prompt reaching the agent while the first is still going.
	time.Sleep(100 * time.Millisecond)
	if got := runner.cancels.Load(); got != 0 {
		t.Fatalf("queued follow-up cancelled the running turn (%d cancels)", got)
	}
	if got := runner.seen(); len(got) != 1 {
		t.Fatalf("queued follow-up ran early: %v", got)
	}

	close(runner.done)
	if err := <-first; err != nil {
		t.Fatalf("first turn = %v, want a clean finish", err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("queued turn = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued turn never ran")
	}
	got := runner.seen()
	if len(got) != 2 || !strings.Contains(got[1], "接着做第二件") || strings.Contains(got[1], "+") {
		t.Fatalf("agent saw %v, want the second prompt with the prefix stripped", got)
	}
}
