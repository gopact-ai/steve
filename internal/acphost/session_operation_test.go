package acphost

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
)

type operationParticipant struct {
	*closeParticipant
	loadEntered   chan struct{}
	loadRelease   chan struct{}
	deleteEntered chan struct{}
	deleteRelease chan struct{}
	notify        bool
	replyOptions  bool
	loadError     error
	emptyNotify   bool
}

func (a *operationParticipant) LoadSession(ctx context.Context, req *acp.LoadSessionRequest) (*acp.LoadSessionResponse, error) {
	return a.restoreSession(ctx, req, "load")
}
func (a *operationParticipant) ResumeSession(ctx context.Context, req *acp.ResumeSessionRequest) (*acp.ResumeSessionResponse, error) {
	resp, err := a.restoreSession(ctx, &acp.LoadSessionRequest{SessionID: req.SessionID, Cwd: req.Cwd, MCPServers: req.MCPServers}, "resume")
	if err != nil {
		return nil, err
	}
	return &acp.ResumeSessionResponse{ConfigOptions: resp.ConfigOptions, Modes: resp.Modes}, nil
}
func (a *operationParticipant) restoreSession(ctx context.Context, req *acp.LoadSessionRequest, method string) (*acp.LoadSessionResponse, error) {
	a.record(method)
	if a.loadError != nil {
		return nil, a.loadError
	}
	if a.notify {
		if a.emptyNotify {
			if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.ConfigOptionUpdateSessionUpdate(nil)}); err != nil {
				return nil, err
			}
			return &acp.LoadSessionResponse{}, nil
		}
		category := acp.SessionConfigOptionCategoryModel
		choices := acp.UngroupedSessionConfigSelectOptions{{Value: "notified", Name: "Notified model"}}
		options := []acp.SessionConfigOption{{ID: "model", Name: "Model", Type: acp.SessionConfigOptionTypeSelect, Category: &category, CurrentValue: acp.SessionConfigValueID("notified"), Options: acp.SessionConfigSelectOptions{Ungrouped: &choices}}}
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.ConfigOptionUpdateSessionUpdate(options)}); err != nil {
			return nil, err
		}
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.CurrentModeUpdateSessionUpdate("notified-mode")}); err != nil {
			return nil, err
		}
	}
	if a.loadEntered != nil {
		close(a.loadEntered)
		<-a.loadRelease
	}
	if method == "load" {
		if err := a.client.Update(context.WithoutCancel(ctx), &acp.SessionNotification{SessionID: req.SessionID, Update: acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("late replay history"))}); err != nil {
			return nil, err
		}
	}
	if a.replyOptions {
		category := acp.SessionConfigOptionCategoryModel
		choices := acp.UngroupedSessionConfigSelectOptions{{Value: "reply", Name: "Reply model"}}
		options := []acp.SessionConfigOption{{ID: "model", Name: "Model", Type: acp.SessionConfigOptionTypeSelect, Category: &category, CurrentValue: acp.SessionConfigValueID("reply"), Options: acp.SessionConfigSelectOptions{Ungrouped: &choices}}}
		return &acp.LoadSessionResponse{ConfigOptions: &options}, nil
	}
	return &acp.LoadSessionResponse{}, nil // Optional fields intentionally omitted.
}
func (a *operationParticipant) DeleteSession(context.Context, *acp.DeleteSessionRequest) (*acp.DeleteSessionResponse, error) {
	a.record("delete")
	if a.deleteEntered != nil {
		close(a.deleteEntered)
		<-a.deleteRelease
	}
	return &acp.DeleteSessionResponse{}, nil
}

type operationTransport struct{ agent *operationParticipant }

func (operationTransport) Name() string { return "deterministic-session-operations" }
func (tr operationTransport) Start(context.Context) (Process, error) {
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
func operationHost(t *testing.T, a *operationParticipant) *Host {
	t.Helper()
	a.closeParticipant = &closeParticipant{lifecycleParticipant: &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31)}, responded: make(chan struct{}, 8)}
	h := New(Config{Transport: operationTransport{agent: a}})
	t.Cleanup(h.Stop)
	t.Cleanup(func() {
		for _, release := range []chan struct{}{a.loadRelease, a.deleteRelease, a.release} {
			if release != nil {
				select {
				case <-release:
				default:
					close(release)
				}
			}
		}
	})
	return h
}
func awaitOperation(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("ACP operation did not dispatch")
	}
}

func TestLoadTimeoutRetainsGateAgainstLateHistory(t *testing.T) {
	a := &operationParticipant{loadEntered: make(chan struct{}), loadRelease: make(chan struct{})}
	h := operationHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, generation, err := h.OpenSession(t.Context(), "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReplayHistory = true
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := h.OpenSession(ctx, sid, cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("load timeout=%v", err)
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: cfg.Workdir}); err == nil {
		t.Error("cached open cleared an unknown load")
	}
	if err := h.CloseSession(t.Context(), sid); err == nil {
		t.Error("close admitted during unknown load")
	}
	if err := h.DeleteSession(t.Context(), sid); err == nil {
		t.Error("delete admitted during unknown load")
	}
	a.promptStarted = make(chan struct{})
	a.promptRelease = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		out, _, err := h.Prompt(t.Context(), sid, generation, "must not replay test input", nil)
		if strings.Contains(out, "late replay") {
			t.Error("late history contaminated new answer")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("prompt admitted during unknown load")
		}
		close(a.loadRelease)
	case <-a.promptStarted:
		close(a.loadRelease)
		awaitOperation(t, a.responded)
		close(a.promptRelease)
		<-done
		t.Error("prompt reached the peer during unknown load")
	case <-time.After(time.Second):
		t.Fatal("prompt guard did not answer")
	}
	awaitOperation(t, a.responded)
	if _, err := h.ListSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: cfg.Workdir}); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("late load ACK unblocked cached open: %v", err)
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "still must not dispatch", nil); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("late load ACK unblocked prompt: %v", err)
	}
	if a.calls.Load() != 0 || a.process.kills.Load() != 0 {
		t.Error("unknown load submitted close or killed shared host")
	}
}
func TestDeleteFirstExcludesWarmReplay(t *testing.T) {
	a := &operationParticipant{deleteEntered: make(chan struct{}), deleteRelease: make(chan struct{})}
	h := operationHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, _, err := h.OpenSession(t.Context(), "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.DeleteSession(t.Context(), sid) }()
	awaitOperation(t, a.deleteEntered)
	cfg.ReplayHistory = true
	if _, _, err := h.OpenSession(t.Context(), sid, cfg); err == nil {
		t.Error("warm replay admitted after delete dispatch")
	}
	close(a.deleteRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", "delete"})
}
func TestLoadNotificationsBeforeOmittedReplyAreRetained(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "warm"
		}
		t.Run(name, func(t *testing.T) {
			a := &operationParticipant{notify: true}
			h := operationHost(t, a)
			cfg := SessionConfig{Workdir: t.TempDir()}
			sid := acp.SessionID("stored")
			if warm {
				var err error
				sid, _, err = h.OpenSession(t.Context(), "", cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg.ReplayHistory = true
			_, generation, err := h.OpenSession(t.Context(), sid, cfg)
			if err != nil {
				t.Fatal(err)
			}
			settings, known := h.SettingsForGeneration(sid, generation)
			if !known || settings.Model != "Notified model" || settings.Mode != "notified-mode" || len(settings.Options) != 1 {
				t.Fatalf("notify-before-reply lost confirmed settings: known=%v settings=%+v", known, settings)
			}
		})
	}
}

func TestCloseFirstExcludesWarmReplay(t *testing.T) {
	a := &operationParticipant{}
	h := operationHost(t, a)
	a.entered = make(chan struct{})
	a.release = make(chan struct{})
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, _, err := h.OpenSession(t.Context(), "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.CloseSession(t.Context(), sid) }()
	awaitOperation(t, a.entered)
	cfg.ReplayHistory = true
	if _, _, err := h.OpenSession(t.Context(), sid, cfg); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("warm replay admitted after close dispatch: %v", err)
	}
	close(a.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if knownSession(h, sid) {
		t.Fatal("close ACK did not retire the original state")
	}
	requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", "close"})
}
func TestDeleteTimeoutRetainsGateAndDoesNotRetry(t *testing.T) {
	a := &operationParticipant{deleteEntered: make(chan struct{}), deleteRelease: make(chan struct{})}
	h := operationHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, _, err := h.OpenSession(t.Context(), "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.DeleteSession(ctx, sid) }()
	awaitOperation(t, a.deleteEntered)
	cancel()
	if err := <-done; !errors.Is(err, ErrSessionOperationUnconfirmed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("unknown delete=%v", err)
	}
	if err := h.DeleteSession(t.Context(), sid); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("unknown delete retried: %v", err)
	}
	cfg.ReplayHistory = true
	if _, _, err := h.OpenSession(t.Context(), sid, cfg); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("delete unknown admitted load: %v", err)
	}
	close(a.deleteRelease)
	awaitOperation(t, a.responded)
	if err := h.DeleteSession(t.Context(), sid); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("unmatched late delete ACK enabled retry: %v", err)
	}
	requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", "delete"})
}
func TestUnknownLoadRequiresOriginalRuntimeStopForRetirement(t *testing.T) {
	a := &operationParticipant{loadEntered: make(chan struct{}), loadRelease: make(chan struct{})}
	h := operationHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, gen, err := h.OpenSession(t.Context(), "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	original := a.process
	cfg.ReplayHistory = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, _, err := h.OpenSession(ctx, sid, cfg); done <- err }()
	awaitOperation(t, a.loadEntered)
	cancel()
	if err := <-done; !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("unknown load=%v", err)
	}
	close(a.loadRelease)
	awaitOperation(t, a.responded)
	original.lifecycleProcess.Kill()
	waitCloseWatch(t, h)
	if _, _, err := h.OpenSession(t.Context(), sid, cfg); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("watch reset erased load gate: %v", err)
	}
	if _, _, err := h.Prompt(t.Context(), sid, gen, "must not restart", nil); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("unknown load prompt=%v", err)
	}
	if h.generation != gen {
		t.Fatal("old unknown load caused a lazy replacement")
	}
	original.stopped.Store(true)
	if err := h.CloseSession(t.Context(), sid); err != nil {
		t.Fatalf("original positive stop failed to retire: %v", err)
	}
	requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", "load"})
}
func TestLoadResponsePresentFieldsSupersedeEarlierNotifications(t *testing.T) {
	a := &operationParticipant{notify: true, replyOptions: true}
	h := operationHost(t, a)
	sid, gen, err := h.OpenSession(t.Context(), "stored", SessionConfig{Workdir: t.TempDir(), ReplayHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	settings, known := h.SettingsForGeneration(sid, gen)
	if !known || settings.Model != "Reply model" || settings.Mode != "notified-mode" {
		t.Fatalf("present reply field versus omitted mode=%+v known=%v", settings, known)
	}
}
func TestLoadExplicitRejectionReleasesGate(t *testing.T) {
	a := &operationParticipant{loadError: &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "peer rejected load"}}
	h := operationHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, _, err := h.OpenSession(t.Context(), "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReplayHistory = true
	_, _, err = h.OpenSession(t.Context(), sid, cfg)
	var rpc *acp.Error
	if !errors.As(err, &rpc) || errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("explicit load rejection=%v", err)
	}
	h.mu.Lock()
	blocked := h.SessionBlockedLocked(sid)
	h.mu.Unlock()
	if blocked != nil {
		t.Fatalf("rejection retained gate: %v", blocked)
	}
	a.loadError = nil
	if _, _, err := h.OpenSession(t.Context(), sid, cfg); err != nil {
		t.Fatalf("explicit load rejection prevented retry: %v", err)
	}
	requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", "load", "load"})
}
func TestLocalTypedCancelCannotBecomeLoadOrDeleteResponse(t *testing.T) {
	for _, method := range []string{"load", "delete"} {
		t.Run(method, func(t *testing.T) {
			a := &operationParticipant{loadEntered: make(chan struct{}), loadRelease: make(chan struct{}), deleteEntered: make(chan struct{}), deleteRelease: make(chan struct{})}
			h := operationHost(t, a)
			cfg := SessionConfig{Workdir: t.TempDir()}
			sid, gen, err := h.OpenSession(t.Context(), "", cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			done := make(chan error, 1)
			go func() {
				if method == "load" {
					cfg.ReplayHistory = true
					_, _, err := h.OpenSession(ctx, sid, cfg)
					done <- err
				} else {
					done <- h.DeleteSession(ctx, sid)
				}
			}()
			if method == "load" {
				awaitOperation(t, a.loadEntered)
			} else {
				awaitOperation(t, a.deleteEntered)
			}
			cause := &acp.Error{Code: acp.ErrorCodeRequestCanceled, Message: "local typed cancellation"}
			cancel(cause)
			if err := <-done; !errors.Is(err, ErrSessionOperationUnconfirmed) || !errors.Is(err, cause) {
				t.Fatalf("typed local cause=%v", err)
			}
			_, _, err = h.Prompt(t.Context(), sid, gen, "never dispatch", nil)
			if !errors.Is(err, ErrSessionOperationUnconfirmed) || PromptSettled(err) {
				t.Fatalf("blocked prompt fabricated a terminal response: %v", err)
			}
		})
	}
}

func TestLoadResumeNotificationStateAndGateBothRoutes(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "released"
		}
		t.Run(name, func(t *testing.T) {
			a := &operationParticipant{notify: true}
			h := operationHost(t, a)
			cfg := SessionConfig{Workdir: t.TempDir()}
			sid := acp.SessionID("stored")
			if warm {
				var err error
				sid, _, err = h.OpenSession(t.Context(), "", cfg)
				if err != nil {
					t.Fatal(err)
				}
				// A live cached open intentionally sends no RPC. Release the
				// existing context first to exercise an actual resume on wire.
				if err := h.CloseSession(t.Context(), sid); err != nil {
					t.Fatal(err)
				}
			}
			_, gen, err := h.OpenSession(t.Context(), sid, cfg)
			if err != nil {
				t.Fatal(err)
			}
			settings, known := h.SettingsForGeneration(sid, gen)
			if !known || settings.Model != "Notified model" || settings.Mode != "notified-mode" {
				t.Fatalf("resume lost early notification: %+v, %v", settings, known)
			}
		})
	}
}
func TestNotificationEmptySelectorsAreConfirmedNotOmitted(t *testing.T) {
	a := &operationParticipant{notify: true}
	h := operationHost(t, a)
	sid, _, err := h.OpenSession(t.Context(), "stored", SessionConfig{Workdir: t.TempDir(), ReplayHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Options(sid)) != 1 {
		t.Fatal("fixture did not establish selectors")
	}
	a.emptyNotify = true
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: t.TempDir(), ReplayHistory: true}); err != nil {
		t.Fatal(err)
	}
	if options := h.Options(sid); len(options) != 0 {
		t.Fatalf("explicit empty notification was treated as omission: %v", options)
	}
}
func TestMatchedReplyCannotReplaceConfirmedLaterNotification(t *testing.T) {
	a := &operationParticipant{notify: true}
	h := operationHost(t, a)
	sid, gen, err := h.OpenSession(t.Context(), "stored", SessionConfig{Workdir: t.TempDir(), ReplayHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	oldState := h.sessions[sid]
	h.mu.Unlock()
	// A real post-reply notification updates the published state, not a stale
	// response snapshot. Neither generation nor producer identity is fabricated.
	category := acp.SessionConfigOptionCategoryModel
	choices := acp.UngroupedSessionConfigSelectOptions{{Value: "later", Name: "Later model"}}
	options := []acp.SessionConfigOption{{ID: "model", Name: "Model", Type: acp.SessionConfigOptionTypeSelect, Category: &category, CurrentValue: acp.SessionConfigValueID("later"), Options: acp.SessionConfigSelectOptions{Ungrouped: &choices}}}
	if err := a.client.Update(t.Context(), &acp.SessionNotification{SessionID: sid, Update: acp.ConfigOptionUpdateSessionUpdate(options)}); err != nil {
		t.Fatal(err)
	}
	// List response's notification barrier proves the earlier notification drained.
	if _, err := h.ListSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	settings, known := h.SettingsForGeneration(sid, gen)
	h.mu.Lock()
	same := h.sessions[sid] == oldState
	h.mu.Unlock()
	if !known || !same || settings.Model != "Later model" {
		t.Fatalf("post-reply notification lost: %+v known=%v same=%v", settings, known, same)
	}
}
