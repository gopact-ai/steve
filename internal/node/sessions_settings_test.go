package node

import (
	"reflect"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

func settingsTestSession(t *testing.T) (*Server, nodewire.SessionRequest, *ownedSession) {
	t.Helper()
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
		Harnesses:         map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}},
		SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.sessions.Close() })
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID = "mock", t.TempDir(), "settings/open"
	opened, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID = opened.ID
	return s, req, s.sessions.sessions[opened.ID]
}

func requireSettingsOption(t *testing.T, settings view.Settings, id, value string) {
	t.Helper()
	for _, option := range settings.Options {
		if option.ID == id && option.Current == value && len(option.Choices) > 0 {
			return
		}
	}
	t.Fatalf("missing confirmed selector %s=%s: %+v", id, value, settings)
}

func TestNodeProgressKeepsAuthoritativeSelectors(t *testing.T) {
	for _, input := range []string{"hello", "switchmodel"} {
		t.Run(input, func(t *testing.T) {
			s, req, one := settingsTestSession(t)
			req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, "settings/input", 1, input
			if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil {
				t.Fatal(err)
			}
			state := waitRecordedInput(t, s.sessions, req)
			one.mu.Lock()
			native := acp.SessionID(one.record.UpstreamID)
			one.mu.Unlock()
			want := one.host.Settings(native)
			if !reflect.DeepEqual(state.Settings, want) || len(state.Settings.Options) == 0 {
				t.Fatalf("progress replaced the authoritative settings: got %+v want %+v", state.Settings, want)
			}
			option, choices := one.host.ModelChoices(native)
			if state.ModelOption != string(option) || !reflect.DeepEqual(state.ModelChoices, choices) {
				t.Fatal("model choices diverged from the current native selectors")
			}
			saved := savedProgress(t, one)
			if !reflect.DeepEqual(saved.State.Settings, want) {
				t.Fatal("durable selectors differ from the live native session")
			}
			if input == "hello" {
				// Idle preference changes must not rewrite the completed
				// turn's presentation or receipt as if it ran in the new mode.
				progress, receipt := state.Progress, state.Command.Receipt
				req.Action, req.OptionID, req.OptionValue = nodewire.SessionActionOption, "mode", "read-only"
				changed, err := s.sessions.Do(t.Context(), "cluster-1", req)
				if err != nil {
					t.Fatal(err)
				}
				requireSettingsOption(t, changed.Settings, "mode", "read-only")
				if !reflect.DeepEqual(changed.Progress, progress) || changed.Command.Receipt != receipt {
					t.Fatal("idle selector change rewrote completed turn evidence")
				}
				want = one.host.Settings(native)
			}
			// Closing the process must retain its last confirmed selectors,
			// not overwrite them with an empty host lookup.
			req.Action = nodewire.SessionActionClose
			closed, err := s.sessions.Do(t.Context(), "cluster-1", req)
			if err != nil || !reflect.DeepEqual(closed.Settings, want) {
				t.Fatalf("process stop erased confirmed selectors: %+v %v", closed.Settings, err)
			}
		})
	}
}

func TestNodeCoalescedProgressCannotUndoConfirmedOption(t *testing.T) {
	s, req, one := settingsTestSession(t)
	stale := one.host.Settings(acp.SessionID(one.record.UpstreamID))
	one.mu.Lock()
	// This is the deterministic boundary where a running turn has queued
	// an older progress snapshot while an option RPC is being processed.
	next := one.copyLocked()
	next.State.State = nodewire.SessionRunning
	next.State.InputAccepted = 1
	next.CurrentCommand = "settings/input"
	next.Commands[next.CurrentCommand] = nodewire.SessionCommand{
		ID: next.CurrentCommand, InputSequence: 1, State: nodewire.SessionCommandRunning, DispatchState: "dispatched",
	}
	next.CommandHashes[next.CurrentCommand] = "settings-input-hash"
	if err := one.commitLocked(next); err != nil {
		one.mu.Unlock()
		t.Fatal(err)
	}
	one.pendingProgress = &view.Progress{Answer: "pending answer", Settings: stale}
	one.mu.Unlock()
	req.Action, req.CommandID, req.OptionID, req.OptionValue = nodewire.SessionActionOption, "settings/input", "mode", "read-only"
	changed, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	requireSettingsOption(t, changed.Settings, "mode", "read-only")
	requireSettingsOption(t, changed.Progress.Settings, "mode", "read-only")
	if changed.Progress.Answer != "pending answer" {
		t.Fatal("option confirmation dropped pending progress")
	}
	// A late callback with the old snapshot is not a new native setting.
	if err := one.updateProgress(req.CommandID, view.Progress{Answer: "late answer", Settings: stale}); err != nil {
		t.Fatal(err)
	}
	one.mu.Lock()
	err = one.commitLocked(one.copyLocked())
	one.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	requireSettingsOption(t, savedProgress(t, one).State.Settings, "mode", "read-only")
}
