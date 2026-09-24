package turn

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

// waitDeadline bounds how long a test waits for a goroutine to reach a known
// point. A passing test never spends it; a generous bound is what keeps the
// suite usable under -race, where everything runs several times slower.
const waitDeadline = 10 * time.Second

type fakeManager struct {
	runners map[string]*fakeRunner
	opened  []string
	// placed records where each session was asked to run, so a test can
	// assert an agent reached its node rather than the hub.
	placed   []harness.Placement
	workdirs []string
	servers  [][]acp.MCPServer
	fail     error
	failOnce bool
	mcpHTTP  bool
	// opening, when set, receives once OpenSession is reached; OpenSession
	// then waits until proceed is closed.
	opening chan struct{}
	proceed chan struct{}
}

func (m *fakeManager) OpenSession(_ context.Context, at harness.Placement, upstreamID, workdir string, servers []acp.MCPServer) (harness.Runner, error) {
	if m.opening != nil {
		m.opening <- struct{}{}
		<-m.proceed
	}
	if m.fail != nil {
		err := m.fail
		if m.failOnce {
			m.fail = nil
		}
		return nil, err
	}
	id := upstreamID
	if id == "" {
		id = at.Harness + "-session"
	}
	runner := m.runners[at.Harness]
	runner.id = id
	m.opened = append(m.opened, at.Harness+":"+upstreamID)
	m.placed = append(m.placed, at)
	m.workdirs = append(m.workdirs, workdir)
	m.servers = append(m.servers, servers)
	return runner, nil
}

func (m *fakeManager) CloseSession(context.Context, harness.Placement, string) error { return nil }

func (m *fakeManager) SupportsHTTPMCP(context.Context, harness.Placement) (bool, error) {
	return m.mcpHTTP, nil
}

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
	// cancelSettles answers a cancel the way a harness does: the prompt
	// settles with ErrTurnCanceled, the agent having acknowledged the stop.
	// Without it a cancel is a local context.Canceled, never settled.
	cancelSettles bool
	// onCancel runs inside Cancel, before the prompt is let go: what the
	// agent manages to do while the stop is under way.
	onCancel func()
	// cancelErr is what Cancel answers: an agent that did not accept the
	// stop. The prompt is still let go.
	cancelErr error
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
		if r.cancelSettles {
			return "", nil, harness.ErrTurnCanceled
		}
		return "", nil, context.Canceled
	}
	if r.reply != "" {
		return r.reply, append([]string{}, r.activity...), r.err
	}
	return "reply: " + prompt, nil, r.err
}
func (r *fakeRunner) Cancel(context.Context) error {
	r.cancels.Add(1)
	if r.onCancel != nil {
		r.onCancel()
	}
	r.canceled.Store(true)
	if r.done != nil {
		r.stop.Do(func() { close(r.done) })
	}
	return r.cancelErr
}
func (r *fakeRunner) Abort() { r.aborts.Add(1) }

func TestCoordinatorPreservesSessionOnTurnErrorWithoutKillingProcess(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", SystemPrompt: "rules", Default: true},
	})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{err: errors.New("prompt failed")}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err == nil {
		t.Fatal("expected prompt error")
	}
	// The turn ended with an error while the context was intact: the process
	// is healthy and must not be canceled or killed.
	if runner.cancels.Load() != 0 || runner.aborts.Load() != 0 {
		t.Fatalf("healthy process was torn down: cancels=%d aborts=%d", runner.cancels.Load(), runner.aborts.Load())
	}
	if saved, ok := store.Conversation("chat").Sessions["codex"]; !ok || saved.UpstreamID == "" {
		t.Fatal("failed turn lost its native context")
	}
}

func TestCoordinatorTimeoutDoesNotAbortSharedProcess(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	// The idle clock starts before the workspace resolves and the session
	// opens, so a budget short enough to run out during that setup never
	// reaches the prompt and aborts nothing. On a loaded machine under
	// -race that setup takes hundreds of milliseconds; give it room. The
	// runner blocks until the deadline fires inside Prompt, so this is the
	// timeout expiring on a stuck turn, not on a slow start.
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, 2*time.Second)
	if _, err := handle(coordinator, t.Context(), "stuck"); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if got := runner.seen(); len(got) != 1 {
		t.Fatalf("the turn timed out before reaching the agent: prompts = %v", got)
	}
	if runner.aborts.Load() != 0 || runner.cancels.Load() != 0 {
		t.Fatal("one expired turn tore down or re-cancelled the shared process")
	}
	if saved, ok := store.Conversation("chat").Sessions["codex"]; !ok || saved.UpstreamID == "" {
		t.Fatal("timed-out turn lost its native context")
	}
}

// manualClock is a prompt idle clock whose time passes only in advance.
type manualClock struct {
	context.Context
	done    chan struct{}
	limit   time.Duration
	mu      sync.Mutex
	err     error
	silence time.Duration
}

// manualClocks installs manual idle clocks on c and hands each one created
// to the test.
func manualClocks(c *Coordinator) <-chan *manualClock {
	clocks := make(chan *manualClock, 1)
	c.promptClock.start = func(parent context.Context, d time.Duration) (idle.Context, func(), func()) {
		clock := &manualClock{Context: parent, done: make(chan struct{}), limit: d}
		context.AfterFunc(parent, func() { clock.finish(parent.Err()) })
		clocks <- clock
		return clock, func() { clock.finish(context.Canceled) }, clock.touch
	}
	return clocks
}

func (c *manualClock) Done() <-chan struct{} { return c.done }
func (c *manualClock) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
func (c *manualClock) Pause()  {}
func (c *manualClock) Resume() {}

func (c *manualClock) touch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.silence = 0
}

// advance passes d of silence; the clock expires once the silence since the
// last touch reaches its limit.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.silence += d
	expired := c.silence >= c.limit
	c.mu.Unlock()
	if expired {
		c.finish(context.DeadlineExceeded)
	}
}

func (c *manualClock) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitDeadline):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// The silence the prompt timeout measures is the agent's: preparation
// before the prompt is sent does not use up the agent's allowance.
func TestPromptSilenceIsMeasuredFromThePromptNotFromPreparation(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	const limit = time.Minute
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}, opening: make(chan struct{}, 1), proceed: make(chan struct{})}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, limit)
	clocks := manualClocks(coordinator)
	done := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "slow start")
		done <- err
	}()
	clock := receive(t, clocks, "the prompt's idle clock")
	receive(t, manager.opening, "the session to open")
	// Preparation and the agent's silent answer each stay inside the
	// limit; together they exceed it.
	clock.advance(limit * 3 / 4)
	close(manager.proceed)
	receive(t, runner.started, "the prompt")
	clock.advance(limit * 3 / 4)
	runner.stop.Do(func() { close(runner.done) })
	if err := receive(t, done, "the turn"); err != nil {
		t.Fatalf("preparation time was charged to the agent's silence: %v", err)
	}
}

// Preparation runs under the prompt timeout: a turn whose preparation
// alone outlasts it ends without sending the prompt.
func TestPreparationOutlastingPromptTimeoutEndsTheTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	const limit = time.Minute
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}, opening: make(chan struct{}, 1), proceed: make(chan struct{})}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, limit)
	clocks := manualClocks(coordinator)
	done := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "stuck start")
		done <- err
	}()
	clock := receive(t, clocks, "the prompt's idle clock")
	receive(t, manager.opening, "the session to open")
	clock.advance(limit)
	close(manager.proceed)
	if err := receive(t, done, "the turn"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("preparation outlasting the prompt timeout: %v", err)
	}
	if got := runner.seen(); len(got) != 0 {
		t.Fatalf("the prompt was sent after preparation outlasted the timeout: %v", got)
	}
}

func TestCoordinatorKeepsContextWhenResumeFails(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	assembler := capability.NewAssembler(nil)
	capabilities, err := assembler.Assemble(catalog.Default())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, _ := state.OpenLedger(testLedger(t))
	if err := store.SaveSession(state.Session{
		ConversationID: "chat", AgentID: "codex", HarnessID: "codex",
		UpstreamID: "stale-session", Workspace: dir,
		CapabilityHash: capabilities.Fingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	failing := &fakeManager{fail: errors.New("session/load: not found")}
	coordinator := newCoordinatorIn(t, map[string]string{"codex": dir}, catalog, store, assembler, failing, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err == nil {
		t.Fatal("expected open error")
	}
	if saved, ok := store.Conversation("chat").Sessions["codex"]; !ok || saved.UpstreamID != "stale-session" {
		t.Fatal("resume failure discarded the native context")
	}
}

func TestCoordinatorNeverFallsBackToNewSessionAfterLoadFails(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	assembler := capability.NewAssembler(nil)
	capabilities, err := assembler.Assemble(catalog.Default())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, _ := state.OpenLedger(testLedger(t))
	if err := store.SaveSession(state.Session{
		ConversationID: "chat", AgentID: "codex", HarnessID: "codex",
		UpstreamID: "stale-session", Workspace: dir,
		CapabilityHash: capabilities.Fingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{
		fail:     errors.New("session/load: authentication required"),
		failOnce: true,
		runners:  map[string]*fakeRunner{"codex": {reply: "recovered"}},
	}
	coordinator := newCoordinatorIn(t, map[string]string{"codex": dir}, catalog, store, assembler, manager, time.Minute)
	result, err := handle(coordinator, t.Context(), "hello")
	if err == nil || result.Text != "" {
		t.Fatalf("resume failure was hidden: %#v, %v", result, err)
	}
	if len(manager.opened) != 0 || len(manager.runners["codex"].seen()) != 0 {
		t.Fatalf("opens = %v", manager.opened)
	}
}

func TestCoordinatorStatusHasFields(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), &fakeManager{}, time.Minute)
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
	store, _ := state.OpenLedger(testLedger(t))
	manager := &blockingOpenManager{started: make(chan struct{})}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
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

func (m *blockingOpenManager) OpenSession(ctx context.Context, _ harness.Placement, _, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingOpenManager) CloseSession(context.Context, harness.Placement, string) error {
	return nil
}

func (m *blockingOpenManager) SupportsHTTPMCP(context.Context, harness.Placement) (bool, error) {
	return false, nil
}

func TestCoordinatorPendingCancelStopsNextTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
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
		"codex": {Harness: "codex", Default: true},
	})
	dir := t.TempDir()
	store, _ := state.OpenLedger(testLedger(t))
	if err := store.SaveSession(state.Session{
		ConversationID: "chat", AgentID: "codex", HarnessID: "codex", Workspace: dir,
		CapabilityHash: "ignored", Tainted: true,
	}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := newCoordinatorIn(t, map[string]string{"codex": dir}, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err == nil || !strings.Contains(err.Error(), "/new") {
		t.Fatalf("expected tainted session error, got %v", err)
	}
}

func TestCoordinatorSwitchesAgentsAndRestoresSessions(t *testing.T) {
	t.Parallel()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex":  {Harness: "codex", Default: true},
		"claude": {Harness: "claude", SystemPrompt: "Act as Claude."},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}, "claude": {}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)

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
	store, _ := state.OpenLedger(testLedger(t))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	conversation := store.Conversation("chat")
	session := conversation.Sessions["codex"]
	session.CapabilityHash = "stale"
	session.SessionConfigHash = "changed-session-configuration"
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
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
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
	if saved, ok := store.Conversation("chat").Sessions["codex"]; !ok || saved.UpstreamID == "" {
		t.Fatal("cancel discarded the conversation context")
	}
}

func TestCoordinatorCancelWithoutRunningTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil || result.Text != i18n.New(i18n.LocaleZH).T(i18n.NoRunningTurn) {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
}

func TestCoordinatorKeepsSessionWhenAgentCancelsTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{err: fmt.Errorf("%w: %w", harness.ErrTurnCanceled, context.Canceled)}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
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
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{id: "sess-1", err: harness.ErrTurnCanceled}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
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

// A turn the agent never settled preserves a tainted native context; it must
// not silently start over on the next message.
func TestAbandonedTurnPreservesTaintedContext(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{id: "sess-1", err: context.Canceled}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "first"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if saved := store.Conversation("chat").Sessions["codex"]; saved.UpstreamID == "" || !saved.Tainted {
		t.Fatal("abandoned turn lost its context or uncertainty marker")
	}
}

func TestCoordinatorSwitchOnlyAndNew(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true}, "claude": {Harness: "claude"},
	})
	store, _ := state.OpenLedger(testLedger(t))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}, "claude": {}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)

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
	store, _ := state.OpenLedger(testLedger(t))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}, "claude": {}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute,
		withDeps(func(d *Deps) { d.Text = i18n.New(i18n.LocaleEN) }))
	result, err := handle(coordinator, t.Context(), "/use claude")
	if err != nil || result.Text != i18n.New(i18n.LocaleEN).T(i18n.Switched, "claude") {
		t.Fatalf("en switch = %#v, %v", result, err)
	}
}

func handle(c *Coordinator, ctx context.Context, input string) (Result, error) {
	return c.Handle(ctx, Request{ConversationID: "chat", Input: input})
}

// A bare message waits for the running turn instead of replacing it:
// queueing is the default, so follow-up needs no prefix and nothing the
// agent is doing gets killed by accident.
func TestNewMessageQueuesBehindRunningTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "long running")
		first <- err
	}()
	<-runner.started

	second := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "接着做第二件")
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
	if len(got) != 2 || !strings.Contains(got[1], "接着做第二件") {
		t.Fatalf("agent saw %v, want the second prompt after the first", got)
	}
}

// "+" was the queue gesture when interrupting was the default; it stays a
// harmless alias so muscle memory keeps working, and the prefix never
// reaches the agent.
func TestPlusPrefixStillQueuesAndStrips(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := handle(coordinator, t.Context(), "+ 后续任务"); err != nil {
		t.Fatal(err)
	}
	got := runner.seen()
	if len(got) != 1 || strings.Contains(got[0], "+") || !strings.Contains(got[0], "后续任务") {
		t.Fatalf("agent saw %v, want the prompt with the alias stripped", got)
	}
}

// Chinese keyboards produce the fullwidth "！"; it interrupts exactly like
// the halfwidth bang.
func TestFullwidthBangInterrupts(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "long running")
		first <- err
	}()
	<-runner.started

	second := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "！改做这个")
		second <- err
	}()

	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted turn ended with %v, want context.Canceled", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("interrupted turn never returned")
	}
	close(runner.done)
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("interrupting turn failed: %v", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("interrupting turn never returned")
	}
	got := runner.seen()
	if len(got) != 2 || !strings.Contains(got[1], "改做这个") || strings.Contains(got[1], "！") {
		t.Fatalf("agent saw %v, want the second prompt with the bang stripped", got)
	}
}

// TestAgentRunsOnItsConfiguredNode: placement follows the agent's config all
// the way to the runtime, and the session records it so a restart reconnects
// to the same machine. The directory over there is the project's, so the
// conversation is bound to a project homed on that node first.
func TestAgentRunsOnItsConfiguredNode(t *testing.T) {
	t.Parallel()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
		"lab":   {Harness: "codex", Node: "host-3", Aliases: []string{"lab"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := state.OpenLedger(testLedger(t))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}
	labWork := t.TempDir()
	coordinator := newCoordinatorIn(t, map[string]string{"lab": labWork}, catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	// A hub-homed project cannot be worked on from host-3: the refusal
	// names both places and the way out.
	if _, err := handle(coordinator, t.Context(), "@lab do the thing"); err == nil || !strings.Contains(err.Error(), "host-3") || !strings.Contains(err.Error(), "/project") {
		t.Fatalf("expected a not-home refusal naming host-3 and /project, got %v", err)
	}
	if _, err := handle(coordinator, t.Context(), "/project use lab"); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "@lab do the thing"); err != nil {
		t.Fatal(err)
	}
	if len(manager.placed) != 1 {
		t.Fatalf("placements = %v", manager.placed)
	}
	if got := manager.placed[0]; got.Node != "host-3" || got.Harness != "codex" {
		t.Fatalf("placement = %+v, want host-3/codex", got)
	}
	// A remote workspace stays the node's own path, untouched by the hub.
	if manager.workdirs[0] != labWork {
		t.Fatalf("remote workspace = %q, want the project's home on the node", manager.workdirs[0])
	}
	saved := store.Conversation("chat").Sessions["lab"]
	if saved.NodeID != "host-3" || saved.ProjectID != "lab" || saved.ProjectVersion != 2 {
		t.Fatalf("session = %+v, want host-3 under project lab v2", saved)
	}

	// A hub-local agent keeps an empty node rather than inheriting one —
	// once the conversation is back on a hub-homed project.
	if _, err := handle(coordinator, t.Context(), "/project use codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "@codex and this"); err != nil {
		t.Fatal(err)
	}
	if got := manager.placed[1]; got.Node != "" {
		t.Fatalf("local placement = %+v, want no node", got)
	}
}
