package acphost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
)

func TestWarmRestoreNotificationsSurviveExplicitRejection(t *testing.T) {
	a := &operationParticipant{notify: true, rejectAfterNotify: &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "restore rejected after independent settings confirmation"}}
	h := operationHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, gen, err := h.OpenSession(t.Context(), "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReplayHistory = true
	_, _, err = h.OpenSession(t.Context(), sid, cfg)
	var response *acp.Error
	if !errors.As(err, &response) {
		t.Fatalf("expected peer rejection, got %v", err)
	}
	settings, known := h.SettingsForGeneration(sid, gen)
	if !known || settings.Model != "Notified model" || settings.Mode != "notified-mode" {
		t.Fatalf("matched rejection rolled back independent notification: %+v known=%v", settings, known)
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: cfg.Workdir}); err != nil {
		t.Fatalf("explicit rejection was turned into an unknown gate: %v", err)
	}
}

// This is the dispatch/finish adapter the typed owner will integrate. Request
// construction here is only a select fixture, not a rewrite of typed SetOption.
func runReservedConfig(h *Host, ctx context.Context, sid acp.SessionID, generation uint64) error {
	h.mu.Lock()
	op, err := h.beginConfigOperationLocked(ctx, sid, generation)
	caller := h.caller
	h.mu.Unlock()
	if err != nil {
		return err
	}
	req := acp.ValueIDSetSessionConfigOptionRequest(sid, "model", "configured")
	resp, err := caller.SetSessionConfigOption(ctx, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		applyOpenResponse(op.state, nil, &resp.ConfigOptions)
	}
	return h.finishConfigOperationLocked(ctx, sid, op, err)
}
func TestConfigReservationExcludesLifecycleAcrossRPC(t *testing.T) {
	a := &operationParticipant{configEntered: make(chan struct{}), configRelease: make(chan struct{})}
	h := operationHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, gen, err := h.OpenSession(t.Context(), "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runReservedConfig(h, t.Context(), sid, gen) }()
	awaitOperation(t, a.configEntered)
	if err := h.CloseSession(t.Context(), sid); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("close overlapped configure: %v", err)
	}
	if err := h.DeleteSession(t.Context(), sid); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("delete overlapped configure: %v", err)
	}
	cfg.ReplayHistory = true
	if _, _, err := h.OpenSession(t.Context(), sid, cfg); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("load overlapped configure: %v", err)
	}
	if _, _, err := h.Prompt(t.Context(), sid, gen, "must not become a new source", nil); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("new prompt overlapped configure: %v", err)
	}
	requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", "configure"})
	close(a.configRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	settings, known := h.SettingsForGeneration(sid, gen)
	if !known || settings.Model != "Configured model" {
		t.Fatalf("configuration reply not applied: %+v known=%v", settings, known)
	}
}
func TestConfigReservationRejectsLifecycleFirst(t *testing.T) {
	for _, method := range []string{"load", "close", "delete"} {
		t.Run(method, func(t *testing.T) {
			a := &operationParticipant{loadEntered: make(chan struct{}), loadRelease: make(chan struct{}), deleteEntered: make(chan struct{}), deleteRelease: make(chan struct{})}
			h := operationHost(t, a)
			a.entered = make(chan struct{})
			a.release = make(chan struct{})
			cfg := SessionConfig{Workdir: t.TempDir()}
			sid, gen, err := h.OpenSession(t.Context(), "", cfg)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				switch method {
				case "load":
					cfg.ReplayHistory = true
					_, _, err := h.OpenSession(t.Context(), sid, cfg)
					done <- err
				case "close":
					done <- h.CloseSession(t.Context(), sid)
				case "delete":
					done <- h.DeleteSession(t.Context(), sid)
				}
			}()
			var entered, release chan struct{}
			switch method {
			case "load":
				entered, release = a.loadEntered, a.loadRelease
			case "close":
				entered, release = a.entered, a.release
			case "delete":
				entered, release = a.deleteEntered, a.deleteRelease
			}
			awaitOperation(t, entered)
			if err := runReservedConfig(h, t.Context(), sid, gen); !errors.Is(err, ErrSessionBusy) {
				t.Fatalf("configure admitted behind lifecycle: %v", err)
			}
			requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", method})
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestConfigReservationAllowsExistingPromptAndReverseCallback(t *testing.T) {
	a := &operationParticipant{configEntered: make(chan struct{}), configRelease: make(chan struct{}), configReverse: true}
	h := operationHost(t, a)
	a.promptStarted = make(chan struct{})
	a.promptRelease = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-a.promptRelease:
		default:
			close(a.promptRelease)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	sid, gen, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	prompt := make(chan error, 1)
	go func() {
		out, _, err := h.Prompt(ctx, sid, gen, "original running test turn", nil)
		if err == nil && out != "fresh answer" {
			t.Errorf("config gate swallowed original turn output: %q", out)
		}
		prompt <- err
	}()
	awaitOperation(t, a.promptStarted)
	config := make(chan error, 1)
	go func() { config <- runReservedConfig(h, ctx, sid, gen) }()
	awaitOperation(t, a.configEntered)
	// Keep the configuration RPC pending while the original prompt settles.
	close(a.promptRelease)
	if err := <-prompt; err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Prompt(ctx, sid, gen, "not a new source", nil); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("pending config admitted another turn: %v", err)
	}
	close(a.configRelease)
	if err := <-config; err != nil {
		t.Fatal(err)
	}
}
func TestConfigReservationUnknownExcludesNewSources(t *testing.T) {
	a := &operationParticipant{configEntered: make(chan struct{}), configRelease: make(chan struct{})}
	h := operationHost(t, a)
	sid, gen, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { done <- runReservedConfig(h, ctx, sid, gen) }()
	awaitOperation(t, a.configEntered)
	cause := &acp.Error{Code: acp.ErrorCodeRequestCanceled, Message: "local config cancellation"}
	cancel(cause)
	if err := <-done; !errors.Is(err, ErrSessionOperationUnconfirmed) || !errors.Is(err, cause) {
		t.Fatalf("config unknown=%v", err)
	}
	if err := runReservedConfig(h, t.Context(), sid, gen); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("unknown config was reissued: %v", err)
	}
	if err := h.CloseSession(t.Context(), sid); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("close overlapped config unknown: %v", err)
	}
	close(a.configRelease)
	if _, _, err := h.Prompt(t.Context(), sid, gen, "must not replay", nil); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("late config ACK enabled new prompt: %v", err)
	}
	requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", "configure"})
}
