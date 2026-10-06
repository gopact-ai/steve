package acphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
)

// closeParticipant uses the real SDK in both directions. A delayed close
// deliberately ignores request cancellation, as a peer may have already
// released resources before its response reaches the client.
type closeParticipant struct {
	*lifecycleParticipant
	entered     chan struct{}
	release     chan struct{}
	responded   chan struct{}
	calls       atomic.Int32
	rejectFirst bool
	process     *closeProcess
}

func (a *closeParticipant) CloseSession(context.Context, *acp.CloseSessionRequest) (*acp.CloseSessionResponse, error) {
	a.record("close")
	n := a.calls.Add(1)
	if a.rejectFirst && n == 1 {
		return nil, &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "explicit close rejection"}
	}
	if n == 1 && a.entered != nil {
		close(a.entered)
		<-a.release
	}
	return &acp.CloseSessionResponse{}, nil
}

type closeWireWriter struct {
	output    io.Writer
	responded chan struct{}
}

func (w closeWireWriter) Write(raw []byte) (int, error) {
	n, err := w.output.Write(raw)
	if err == nil && w.responded != nil {
		var frame struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(raw, &frame) == nil && string(frame.Result) == "{}" {
			select {
			case w.responded <- struct{}{}:
			default:
			}
		}
	}
	return n, err
}

type closeTransport struct{ agent *closeParticipant }

func (closeTransport) Name() string { return "deterministic-unknown-close" }
func (tr closeTransport) Start(context.Context) (Process, error) {
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	conn, err := acp.NewAgent(input, closeWireWriter{output: output, responded: tr.agent.responded}, func(client *acp.ClientCaller) acp.AgentHandler { tr.agent.client = client; return tr.agent })
	if err != nil {
		_ = input.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, err
	}
	p := &closeProcess{lifecycleProcess: &lifecycleProcess{conn: conn, stdin: stdin, stdout: stdout, output: output}}
	tr.agent.process = p
	return p, nil
}

// Ending the ACP stream is not proof that everything owned by the original
// process stopped. Tests publish that separate evidence explicitly.
type closeProcess struct {
	*lifecycleProcess
	stopped atomic.Bool
	kills   atomic.Int32
}

func (p *closeProcess) Stopped() bool { return p.stopped.Load() }
func (p *closeProcess) Kill()         { p.kills.Add(1); p.lifecycleProcess.Kill() }
func newCloseParticipant(t *testing.T, delay, reject bool) (*Host, *closeParticipant, acp.SessionID, uint64) {
	t.Helper()
	a := &closeParticipant{lifecycleParticipant: &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31)}, rejectFirst: reject, responded: make(chan struct{}, 4)}
	if delay {
		a.entered = make(chan struct{})
		a.release = make(chan struct{})

	}
	h := New(Config{Transport: closeTransport{agent: a}})
	t.Cleanup(h.Stop)
	if delay {
		t.Cleanup(func() {
			select {
			case <-a.release:
			default:
				close(a.release)
			}
		})
	}
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return h, a, sid, generation
}
func cancelWireClose(t *testing.T, h *Host, a *closeParticipant, sid acp.SessionID) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.CloseSession(ctx, sid) }()
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("close did not reach ACP participant")
	}
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("cancel did not bound close")
		return nil
	}
}

func TestUnknownCloseSecondWireRequestIsRefused(t *testing.T) {
	h, a, sid, _ := newCloseParticipant(t, true, false)
	if err := cancelWireClose(t, h, a, sid); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled close=%v", err)
	}
	if err := h.CloseSession(t.Context(), sid); err == nil {
		t.Error("unknown close silently retried")
	}
	if got := a.calls.Load(); got != 1 {
		t.Fatalf("second close reached the same runtime: wire count=%d", got)
	}
	if a.process.kills.Load() != 0 {
		t.Fatal("unknown close killed the shared host")
	}
}
func TestUnknownCloseLateResponseAfterTimeoutDoesNotEnableRetry(t *testing.T) {
	h, a, sid, _ := newCloseParticipant(t, true, false)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := h.CloseSession(ctx, sid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close timeout=%v", err)
	}
	close(a.release)
	select {
	case <-a.responded:
	case <-time.After(time.Second):
		t.Fatal("late close ACK never reached the wire")
	}
	if err := h.CloseSession(t.Context(), sid); err == nil {
		t.Error("unmatched late ACK was promoted to a retry/close proof")
	}
	if got := a.calls.Load(); got != 1 {
		t.Fatalf("late ACK enabled duplicate close: count=%d", got)
	}
}
func TestUnknownCloseExplicitRejectionCanRetry(t *testing.T) {
	h, a, sid, _ := newCloseParticipant(t, false, true)
	var rpc *acp.Error
	if err := h.CloseSession(t.Context(), sid); !errors.As(err, &rpc) {
		t.Fatalf("explicit rejection=%v", err)
	}
	if !knownSession(h, sid) {
		t.Fatal("explicit rejection forgot original session")
	}
	if err := h.CloseSession(t.Context(), sid); err != nil {
		t.Fatalf("explicit rejection could not be retried: %v", err)
	}
	if a.calls.Load() != 2 || knownSession(h, sid) {
		t.Fatal("explicit retry did not complete exactly one later close")
	}
}
func TestUnknownCloseRefusesOpenAndPromptReuse(t *testing.T) {
	h, a, sid, generation := newCloseParticipant(t, true, false)
	if err := cancelWireClose(t, h, a, sid); err == nil {
		t.Fatal("close unexpectedly settled")
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: t.TempDir()}); err == nil {
		t.Error("cached open reused a session with unknown close")
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: t.TempDir(), ReplayHistory: true}); err == nil {
		t.Error("history replay reused a session with unknown close")
	}
	before := a.methods()
	_, _, _ = h.Prompt(t.Context(), sid, generation, "must not dispatch test input", nil)
	if len(a.methods()) != len(before) {
		t.Fatal("prompt dispatched while close was unknown")
	}
}

func waitCloseWatch(t *testing.T, h *Host) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		alive := h.alive
		h.mu.Unlock()
		if !alive {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("process watcher did not retire its session maps")
}
func TestUnknownCloseEOFAndWatchResetRetainOriginalGate(t *testing.T) {
	h, a, sid, generation := newCloseParticipant(t, true, false)
	original := a.process
	done := make(chan error, 1)
	go func() { done <- h.CloseSession(t.Context(), sid) }()
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("close not dispatched")
	}
	_ = original.output.Close() // Real ACP response-stream EOF, not a close ACK.
	select {
	case err := <-done:
		if !errors.Is(err, ErrCloseUnconfirmed) || !errors.Is(err, io.EOF) {
			t.Fatalf("EOF close=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EOF did not end pending RPC")
	}
	close(a.release)
	original.lifecycleProcess.Kill() // Stream exit without native stop evidence.
	waitCloseWatch(t, h)
	if knownSession(h, sid) {
		t.Fatal("fixture did not exercise the watcher map reset")
	}
	if h.ProcessStopped(generation) {
		t.Fatal("stream EOF invented process-stop evidence")
	}
	if err := h.CloseSession(t.Context(), sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("watcher reset erased unknown close: %v", err)
	}
	before := h.generation
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("unknown open=%v", err)
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "never replay", nil); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("unknown prompt=%v", err)
	}
	if h.generation != before {
		t.Fatal("blocked SID lazily restarted the host")
	}
	if a.calls.Load() != 1 {
		t.Fatal("EOF close was redispatched")
	}
	original.stopped.Store(true)
	if !h.ProcessStopped(generation) {
		t.Fatal("original stop evidence was not recognized")
	}
	if err := h.CloseSession(t.Context(), sid); err != nil {
		t.Fatalf("confirmed original cleanup could not retire close: %v", err)
	}
	h.mu.Lock()
	blocked := h.SessionBlockedLocked(sid)
	h.mu.Unlock()
	if blocked != nil || a.calls.Load() != 1 {
		t.Fatal("cleanup retirement sent another close RPC or retained the gate")
	}
}
func TestUnknownCloseNewGenerationDoesNotSettleOldRuntime(t *testing.T) {
	h, a, sid, generation := newCloseParticipant(t, true, false)
	original := a.process
	if err := cancelWireClose(t, h, a, sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("canceled close=%v", err)
	}
	close(a.release)
	original.lifecycleProcess.Kill()
	waitCloseWatch(t, h)
	_, newGeneration, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if newGeneration == generation || a.process == original {
		t.Fatal("fixture did not create a replacement generation")
	}
	a.process.stopped.Store(true)
	a.process.lifecycleProcess.Kill()
	waitCloseWatch(t, h)
	if !h.ProcessStopped(newGeneration) {
		t.Fatal("replacement generation stop was not recognized")
	}
	if err := h.CloseSession(t.Context(), sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("new generation stop settled old close: %v", err)
	}
	if a.calls.Load() != 1 {
		t.Fatal("old unknown close reached the replacement peer")
	}
	original.stopped.Store(true)
	if err := h.CloseSession(t.Context(), sid); err != nil {
		t.Fatalf("original generation stop failed to retire: %v", err)
	}
}
func TestUnknownCloseMissingProcessMapEntryIsNotProof(t *testing.T) {
	h, a, sid, generation := newCloseParticipant(t, true, false)
	if err := cancelWireClose(t, h, a, sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("canceled close=%v", err)
	}
	h.mu.Lock()
	delete(h.processes, generation)
	delete(h.sessions, sid)
	h.mu.Unlock()
	if err := h.CloseSession(t.Context(), sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("absence of maps invented close settlement: %v", err)
	}
	if a.calls.Load() != 1 || a.process.kills.Load() != 0 {
		t.Fatal("missing map caused unsafe close/kill")
	}
}
func TestUnknownClosePendingGateBlocksConcurrentRPCs(t *testing.T) {
	h, a, sid, generation := newCloseParticipant(t, true, false)
	done := make(chan error, 1)
	go func() { done <- h.CloseSession(t.Context(), sid) }()
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("close not pending")
	}
	if err := h.CloseSession(t.Context(), sid); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("pending second close=%v", err)
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("pending open=%v", err)
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "must not dispatch", nil); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("pending prompt=%v", err)
	}
	// DeleteSession and typed setters consume this same guard under h.mu.
	h.mu.Lock()
	blocked := h.SessionBlockedLocked(sid)
	h.mu.Unlock()
	if !errors.Is(blocked, ErrSessionBusy) || a.calls.Load() != 1 {
		t.Fatal("pending mutation guard did not preserve single dispatch")
	}
	close(a.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if knownSession(h, sid) {
		t.Fatal("matched ACK did not retire session")
	}
}
func TestUnknownCloseGuardDoesNotBlockOtherSessions(t *testing.T) {
	h, a, sid, _ := newCloseParticipant(t, true, false)
	other, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := cancelWireClose(t, h, a, sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("unknown close=%v", err)
	}
	if reopened, _, err := h.OpenSession(t.Context(), other, SessionConfig{Workdir: t.TempDir()}); err != nil || reopened != other {
		t.Fatalf("unrelated session was blocked: %q %v", reopened, err)
	}
	h.mu.Lock()
	blocked := h.SessionBlockedLocked(sid)
	otherBlocked := h.SessionBlockedLocked(other)
	h.mu.Unlock()
	if !errors.Is(blocked, ErrCloseUnconfirmed) || otherBlocked != nil {
		t.Fatal("mutation guard lost SID isolation")
	}
	if a.process.kills.Load() != 0 {
		t.Fatal("one unknown close killed another session")
	}
}

func TestUnknownCloseOwnedProcessHelper(t *testing.T) {
	marker := os.Getenv("STEVE_TEST_UNKNOWN_CLOSE_MARKER")
	if marker == "" {
		return
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	count := 0
	for {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if decoder.Decode(&request) != nil {
			os.Exit(0)
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"close": map[string]any{}}}}
		case "session/new":
			result = map[string]any{"sessionId": "owned-test-session"}
		case "session/close":
			count++
			if os.WriteFile(marker, []byte(fmt.Sprint(count)), 0600) != nil {
				os.Exit(2)
			}
			continue // No close response, including after request cancellation.
		default:
			continue
		}
		if encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}) != nil {
			os.Exit(2)
		}
	}
}
func TestUnknownCloseOriginalOwnedProcessStopRetiresWithoutACK(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "close-count")
	h := New(Config{Command: executable, Args: []string{"-test.run=^TestUnknownCloseOwnedProcessHelper$"}, Env: []string{"STEVE_TEST_UNKNOWN_CLOSE_MARKER=" + marker}, ProcessDir: t.TempDir(), NoRestart: true})
	t.Cleanup(h.Stop)
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.CloseSession(ctx, sid) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("close never reached owned process")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("unknown close=%v", err)
	}
	if h.ProcessStopped(generation) {
		t.Fatal("unknown close ended the owned process")
	}
	if err := h.CloseSession(t.Context(), sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("live original process permitted retry: %v", err)
	}
	h.Stop() // Explicit runtime owner action, never implicit per-session cleanup.
	if !h.ProcessStopped(generation) {
		t.Fatal("original owned process stop was not confirmed")
	}
	if err := h.CloseSession(t.Context(), sid); err != nil {
		t.Fatalf("native cleanup did not retire unknown close: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "1" {
		t.Fatalf("native retirement sent another close: %q, %v", raw, err)
	}
}
func TestUnknownClosePreCanceledContextDoesNotAcquireGate(t *testing.T) {
	h, a, sid, _ := newCloseParticipant(t, false, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.CloseSession(ctx, sid); !errors.Is(err, context.Canceled) || errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("known undispatched cancellation=%v", err)
	}
	h.mu.Lock()
	blocked := h.SessionBlockedLocked(sid)
	h.mu.Unlock()
	if blocked != nil || a.calls.Load() != 0 {
		t.Fatal("pre-canceled call dispatched or acquired an unknown gate")
	}
	if err := h.CloseSession(t.Context(), sid); err != nil {
		t.Fatalf("undispatched call prevented explicit close: %v", err)
	}
}

func TestUnknownCloseLocalCancelCauseIsNotPeerRejection(t *testing.T) {
	h, a, sid, _ := newCloseParticipant(t, true, false)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { done <- h.CloseSession(ctx, sid) }()
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("close not dispatched")
	}
	cause := &acp.Error{Code: acp.ErrorCodeRequestCanceled, Message: "local cancellation cause, not a wire response"}
	cancel(cause)
	if err := <-done; !errors.Is(err, ErrCloseUnconfirmed) || !errors.Is(err, cause) {
		t.Fatalf("local typed cancellation masqueraded as peer response: %v", err)
	}
	if err := h.CloseSession(t.Context(), sid); !errors.Is(err, ErrCloseUnconfirmed) || a.calls.Load() != 1 {
		t.Fatalf("typed local cause enabled close replay: %v count=%d", err, a.calls.Load())
	}
}

func TestUnknownCloseBlocksConfigurationAndProviderDelete(t *testing.T) {
	h, participant, sid, generation := newCloseParticipant(t, true, false)
	if err := cancelWireClose(t, h, participant, sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("close = %v", err)
	}
	if err := h.SetOption(t.Context(), sid, generation, "choice", "value"); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("unknown close allowed configuration RPC: %v", err)
	}
	if err := h.DeleteSession(t.Context(), sid); !errors.Is(err, ErrCloseUnconfirmed) {
		t.Fatalf("unknown close allowed provider delete: %v", err)
	}
	if participant.calls.Load() != 1 {
		t.Fatal("mutation guard replayed a close")
	}
}
