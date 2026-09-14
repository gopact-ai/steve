package node

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func TestSettledAgentExitUsesColdContextWithoutNodeRestart(t *testing.T) {
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
	req.ID, req.Binding.TaskID, req.CommandID = first.ID, "next-task", "next-open"
	second, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || second.ID == first.ID || second.ContextID != old.State.ID {
		t.Fatalf("stopped agent took warm admission: %+v %v", second, err)
	}
	current, _, _ := s.sessions.readRecord(second.ID)
	previous, _, _ := s.sessions.readRecord(first.ID)
	if current.UpstreamID != old.UpstreamID || !reflect.DeepEqual(before.Commands, previous.Commands) || previous.State.Binding != before.State.Binding {
		t.Fatal("exit recovery replaced context or rebound old receipts")
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
