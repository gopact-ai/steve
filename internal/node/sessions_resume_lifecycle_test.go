package node

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func TestSettledAgentExitUsesColdContextWithoutNodeRestart(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "interrupted-settings"}[interrupted], func(t *testing.T) {
			testSettledAgentExit(t, interrupted)
		})
	}
}

func testSettledAgentExit(t *testing.T, interrupted bool) {
	t.Helper()
	cfg, req, old := resumedFixture(t, buildMockAgent(t))
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	first, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	input := req
	input.ID, input.Action, input.CommandID, input.InputSequence, input.Text = first.ID, nodewire.SessionActionPrompt, "settled-input", 1, "retained answer"
	state, err := s.sessions.Do(t.Context(), "cluster-1", input)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if state.Command != nil && state.Command.Settled {
			break
		}
		input.Action, input.After, input.WaitMS = nodewire.SessionActionPoll, state.Sequence, 50
		state, err = s.sessions.Do(t.Context(), "cluster-1", input)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.Command == nil || !state.Command.Settled {
		t.Fatal("prompt did not settle")
	}
	before, _, _ := s.sessions.readRecord(first.ID)
	// Physical exit happens after the settled receipt, with the node still up.
	s.sessions.sessions[first.ID].host.Close()
	if interrupted {
		option := req
		option.ID, option.Action, option.OptionID, option.OptionValue = first.ID, nodewire.SessionActionOption, "mode", "default"
		if _, err := s.sessions.Do(t.Context(), "cluster-1", option); err == nil {
			t.Fatal("stopped agent accepted a settings change")
		}
	}
	req.ID, req.Binding.TaskID, req.CommandID = first.ID, "next-task", "next-open"
	second, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || second.ID == first.ID || second.ContextID != old.State.ID {
		t.Fatalf("stopped agent took warm admission: %+v %v", second, err)
	}
	current, _, _ := s.sessions.readRecord(second.ID)
	previous, _, _ := s.sessions.readRecord(first.ID)
	for id, command := range before.Commands {
		command.ProcessStopped = true
		before.Commands[id] = command
	}
	if current.UpstreamID != old.UpstreamID || !reflect.DeepEqual(before.Commands, previous.Commands) || previous.State.Binding != before.State.Binding {
		t.Fatal("exit recovery replaced context or rebound old receipts")
	}
}

func TestConcurrentLiveOpenAndPromptDoNotBlockNode(t *testing.T) {
	cfg, req, old := resumedFixture(t, buildMockAgent(t))
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { s.sessions.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("concurrent requests prevented node shutdown")
		}
	})
	first, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID = first.ID
	input := req
	input.Action, input.CommandID, input.InputSequence, input.Text = nodewire.SessionActionPrompt, "racing-input", 1, "one accepted input"
	start, done := make(chan struct{}), make(chan struct{})
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			request := req
			if i%2 == 0 {
				request = input
			}
			_, err := s.sessions.Do(t.Context(), "cluster-1", request)
			errs <- err
		}()
	}
	close(start)
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent live opens and prompt retries blocked the node")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if state := s.sessions.sessions[first.ID].state(input.CommandID); state.InputAccepted != 1 {
		t.Fatalf("concurrent retry accepted more than one input: %+v", state)
	}
}

func TestPromptAfterShutdownAdmissionDoesNotRecordAnAcceptedInput(t *testing.T) {
	cfg, req, old := resumedFixture(t, buildMockAgent(t))
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	first, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	one := s.sessions.sessions[first.ID]
	// The request already passed Do's initial check when shutdown begins.
	s.sessions.mu.Lock()
	s.sessions.closed = true
	s.sessions.mu.Unlock()
	t.Cleanup(func() {
		s.sessions.mu.Lock()
		s.sessions.closed = false
		s.sessions.mu.Unlock()
		s.sessions.Close()
	})
	req.ID, req.Action, req.CommandID, req.InputSequence, req.Text = first.ID, nodewire.SessionActionPrompt, "shutdown-input", 1, "must not be accepted"
	if _, err := one.prompt(req); err == nil {
		t.Fatal("shutdown accepted a new prompt")
	}
	state := one.state(req.CommandID)
	if state.InputAccepted != 0 || state.Command != nil || state.State != nodewire.SessionIdle {
		t.Fatal("refused prompt left an accepted input without a worker")
	}
}

func TestFailedLiveHandoffCannotForkFromAnOlderSource(t *testing.T) {
	cfg, req, old := resumedFixture(t, "/missing-native-agent")
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("missing agent unexpectedly opened")
	}
	source, _, _ := s.sessions.readRecord(old.State.ID)
	first, exists, err := s.sessions.readRecord(source.ResumeTarget)
	if err != nil || !exists || !first.State.ProcessStopped || s.sessions.sessions[first.State.ID] == nil {
		t.Fatalf("failed open did not retain its stopped owned session: %v", err)
	}
	req.Binding.TaskID, req.CommandID = "third-task", "third-open"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("unreconciled handoff forked")
	}
	after, _, _ := s.sessions.readRecord(first.State.ID)
	if after.ResumeTarget != "" || len(s.sessions.sessions) != 1 {
		t.Fatal("older source changed a claim still owned by an in-memory session")
	}
}

func TestProvenProcessExitReleasesPluginUseDespiteReceiptFailure(t *testing.T) {
	for _, mode := range []string{"archive", "failed-open", "prepared-cancel"} {
		t.Run(mode, func(t *testing.T) {
			cfg, req, old := resumedFixture(t, buildMockAgent(t))
			s := NewServer(cfg)
			selection := nodeRuntimeFixture(t, s, "")
			runtime, err := s.pluginRuntimePool().Prepare(t.Context(), "native", selection, harness.Config{Command: cfg.Harnesses["mock"].Command, Permission: "read"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.closePluginRuntimes()
			req.Plugin, req.Binding.PluginRuntimeID = runtime.Ref.Clone(), runtime.Ref.ID
			old.State.Plugin, old.State.Binding.PluginRuntimeID = runtime.Ref.Clone(), runtime.Ref.ID
			old.ConfigHash = sessionConfigHash(req)
			saveResumeFixture(t, cfg, old)
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer s.sessions.Close()
			state, err := s.sessions.Do(t.Context(), "cluster-1", req)
			if err != nil {
				t.Fatal(err)
			}
			one := s.sessions.sessions[state.ID]
			one.host.Close()
			if mode == "prepared-cancel" {
				// Preparation may persist a use and then lose the final sync
				// response, leaving no runtime owner and no process to stop.
				next := one.copyLocked()
				next.State.State, next.State.ProcessStopped = nodewire.SessionOpening, true
				if err := one.commitLocked(next); err != nil {
					t.Fatal(err)
				}
				delete(s.sessions.sessions, state.ID)
				req.ID, req.Action = "", nodewire.SessionActionCancelOpen
				if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil {
					t.Fatal(err)
				}
			} else {
				failure := errors.New("session receipt storage unavailable")
				one.mu.Lock()
				one.failure = failure
				one.mu.Unlock()
				if mode == "archive" {
					err = s.sessions.archiveStoppedSession(state.ID)
				} else {
					_, err = one.stateAfterFailedOpen(errors.New("native open failed"))
				}
				if !errors.Is(err, failure) {
					t.Fatalf("receipt failure was hidden: %v", err)
				}
			}
			infos, err := s.pluginStore().RuntimeInfos()
			if err != nil || len(infos) != 1 || len(infos[0].Uses) != 1 || !infos[0].Uses[0].Stopped {
				t.Fatalf("proven exit left a live plugin use: %v", err)
			}
		})
	}
}

func TestRestartReconcilesOnlyProvenStoppedPluginUses(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "stopped"}[stopped], func(t *testing.T) {
			cfg, req, old := resumedFixture(t, buildMockAgent(t))
			s := NewServer(cfg)
			selection := nodeRuntimeFixture(t, s, "")
			runtime, err := s.pluginRuntimePool().Prepare(t.Context(), "native", selection, harness.Config{Command: cfg.Harnesses["mock"].Command, Permission: "read"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.closePluginRuntimes()
			req.Plugin, req.Binding.PluginRuntimeID = runtime.Ref.Clone(), runtime.Ref.ID
			old.State.Plugin, old.State.Binding.PluginRuntimeID, old.State.ProcessStopped = runtime.Ref.Clone(), runtime.Ref.ID, stopped
			old.ConfigHash = sessionConfigHash(req)
			saveResumeFixture(t, cfg, old)
			if err := s.pluginStore().BeginRuntimeUse(t.Context(), runtime.Ref, "session/"+old.State.ID, "session"); err != nil {
				t.Fatal(err)
			}
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer s.sessions.Close()
			infos, err := s.pluginStore().RuntimeInfos()
			if err != nil || len(infos) != 1 || len(infos[0].Uses) != 1 || infos[0].Uses[0].Stopped != stopped {
				t.Fatal("restart lost positive-stop distinction", err)
			}
			if stopped {
				if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil {
					t.Fatal(err)
				}
				s.sessions.Close()
			}
			if err := s.pluginStore().RetireRuntime(t.Context(), runtime.Ref); err != nil {
				t.Fatal(err)
			}
			err = s.pluginStore().RemoveRuntime(t.Context(), runtime.Ref)
			if stopped && err != nil {
				t.Fatal("stopped source stranded plugin retirement", err)
			}
			if !stopped && !errors.Is(err, plugins.ErrRuntimeBusy) {
				t.Fatal("unknown process use allowed removal", err)
			}
		})
	}
}

func TestLostResumeOpenAfterRestartUsesOriginalReconciliationReceipt(t *testing.T) {
	cfg, req, old := resumedFixture(t, buildMockAgent(t))
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	s.sessions.Close()
	s = NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	inspect := req
	inspect.ID, inspect.Action = "", nodewire.SessionActionInspectOpen
	got, err := s.sessions.Do(t.Context(), "cluster-1", inspect)
	if err != nil || got.ID != first.ID || got.OpenReceipt == nil || !got.ProcessStopped || got.InputAccepted != 0 {
		t.Fatalf("lost open could not reconcile exact stopped execution: %+v %v", got, err)
	}
	if len(s.sessions.sessions) != 0 {
		t.Fatal("reconciliation restarted an old execution")
	}
}
