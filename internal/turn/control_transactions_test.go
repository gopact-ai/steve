package turn

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/control"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
	"github.com/gopact-ai/steve/internal/work"
)

type closeHookRuntime struct {
	*nodeSessions
	hook func()
}

func (r *closeHookRuntime) CloseSession(ctx context.Context, at harness.Placement, id string) error {
	if r.hook != nil {
		r.hook()
	}
	return r.nodeSessions.CloseSession(ctx, at, id)
}

func TestResetDoesNotReauthorizeAnAlreadyCommittedClose(t *testing.T) {
	t.Parallel()
	c, sessions, req, _ := retirementCoordinator(t)
	revoked := false
	ctx := control.WithMutationGuard(t.Context(), func(*ledger.Tx) error {
		if revoked {
			return control.ErrDenied
		}
		return nil
	})
	c.runtime = &closeHookRuntime{nodeSessions: sessions, hook: func() { revoked = true }}
	if _, err := c.commands().reset(ctx, req, agent.Agent{ID: "codex"}); err != nil {
		t.Fatalf("an approved native close was refused at finalization: %v", err)
	}
	reopened, err := state.OpenLedger(ledgerOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []*state.Store{c.store, reopened} {
		conversation := store.Conversation(req.Source.ConversationID)
		if len(conversation.Sessions) != 0 || len(conversation.Archived) != 1 || conversation.Archived[0].ClosingAt != "" {
			t.Fatalf("dead live session or unfinished archive: %+v", conversation)
		}
	}
}

func TestResetPersistsItsFenceBeforeCloseAndRecoversFailedFinalization(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"intent write", "final write", "lost reply"} {
		t.Run(failure, func(t *testing.T) {
			// Each case builds its own fixture.
			t.Parallel()
			c, sessions, req, _ := retirementCoordinator(t)
			book := ledgerOf(t, c)
			before := c.store.Conversation(req.Source.ConversationID).Sessions["codex"]
			refuse := func() {
				t.Helper()
				if _, err := book.DB().Exec(`CREATE TRIGGER refuse_close BEFORE INSERT ON bindings WHEN NEW.kind='document' AND NEW.id='state' BEGIN SELECT RAISE(FAIL,'test close write refused'); END`); err != nil {
					t.Fatal(err)
				}
			}
			switch failure {
			case "intent write":
				refuse()
			case "final write":
				c.runtime = &closeHookRuntime{nodeSessions: sessions, hook: refuse}
			case "lost reply":
				sessions.err = errors.New("test close response lost")
			}
			if _, err := c.commands().reset(t.Context(), req, agent.Agent{ID: "codex"}); err == nil {
				t.Fatal("failed close reported success")
			}
			reopened, err := state.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "intent write" {
				if len(sessions.closes) != 0 || !reflect.DeepEqual(before, reopened.Conversation(req.Source.ConversationID).Sessions["codex"]) {
					t.Fatal("native side effect preceded durable intent")
				}
				return
			}
			for _, store := range []*state.Store{c.store, reopened} {
				pending := store.Conversation(req.Source.ConversationID).Sessions["codex"]
				if pending.ClosingAt == "" || pending.UpstreamID != before.UpstreamID || !errors.Is(store.CheckSessionContext(before), state.ErrSessionClosing) {
					t.Fatal("uncertain/dead session could be reused, including after restart")
				}
			}
			if failure == "final write" {
				if _, err := book.DB().Exec(`DROP TRIGGER refuse_close`); err != nil {
					t.Fatal(err)
				}
			}
			// Re-open the durable store as a successor process would. An absent
			// reply confirms the original close without opening another context.
			c.store = reopened
			c.runtime = sessions
			stale := reopened.Conversation(req.Source.ConversationID).Sessions["codex"]
			stale.UpstreamID = "another-native-session"
			closes := len(sessions.closes)
			if _, err := c.commands().retryReset(t.Context(), req, agent.Agent{ID: "codex"}, stale); !errors.Is(err, state.ErrSessionChanged) || len(sessions.closes) != closes {
				t.Fatalf("close retry accepted a different native identity: %v", err)
			}
			sessions.err = &node.SessionError{Code: "absent", Message: "already closed"}
			if _, err := c.commands().reset(t.Context(), req, agent.Agent{ID: "codex"}); err != nil {
				t.Fatalf("close retry could not finalize its original intent: %v", err)
			}
			got := c.store.Conversation(req.Source.ConversationID)
			if len(got.Sessions) != 0 || len(got.Archived) != 1 || got.Archived[0].UpstreamID != before.UpstreamID || got.Archived[0].ClosingAt != "" {
				t.Fatalf("retry lost/duplicated original history: %+v", got)
			}
		})
	}
}

func TestResetRetryFinishesOnlyItsApprovedIntentAfterProjectRevocation(t *testing.T) {
	t.Parallel()
	c, sessions, req, _ := retirementCoordinator(t)
	accounts := &identity.Accounts{Book: ledgerOf(t, c)}
	config := c.owners.Configuration()
	config.Accounts = accounts
	if err := accounts.Bootstrap(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	owners, err := identity.NewOwners(config)
	if err != nil {
		t.Fatal(err)
	}
	c.owners, c.controls.Owners = owners, owners
	c.work = &work.Engine{Book: accounts.Book, Runtime: "close-recovery-fixture"}
	if err := c.projects.SetIdentityPolicy(t.Context(), accounts, req.Source.Channel); err != nil {
		t.Fatal(err)
	}
	req.Actor.ID, req.Input, req.Source.MessageID = "close-caller", "/new", "close-first"
	principal := identity.Account{Channel: req.Source.Channel, ID: req.Actor.ID}.Principal()
	binding, _, err := c.projects.Binding(t.Context(), req.Source.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.projects.Grant(t.Context(), binding.ProjectID, principal, project.RoleWrite, accounts.Owner); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("test close response lost")
	sessions.err = lost
	if _, err := c.Handle(t.Context(), req); !errors.Is(err, lost) {
		t.Fatalf("expected a dispatched close with a lost response: %v", err)
	}
	original := c.store.Conversation(req.Source.ConversationID).Sessions["codex"]
	if original.ClosingAt == "" {
		t.Fatal("missing approved close intent")
	}
	beforeTasks := c.tasks.List(req.Source.ConversationID)
	if _, err := c.projects.Grant(t.Context(), binding.ProjectID, principal, project.RoleNone, accounts.Owner); err != nil {
		t.Fatal(err)
	}
	req.Input, req.Source.MessageID = "@codex /new", "close-retry"
	sessions.err = &node.SessionError{Code: "absent", Message: "already closed"}
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatalf("fresh project policy blocked the exact admitted close: %v", err)
	}
	got := c.store.Conversation(req.Source.ConversationID)
	if len(got.Sessions) != 0 || len(got.Archived) != 1 || got.Archived[0].UpstreamID != original.UpstreamID || !reflect.DeepEqual(beforeTasks, c.tasks.List(req.Source.ConversationID)) {
		t.Fatal("retry changed task scope or failed to archive the exact original session")
	}
	// Finishing the approved action must not grant permission for another one.
	original.ClosingAt = ""
	if err := c.store.SaveSession(original); err != nil {
		t.Fatal(err)
	}
	req.Source.MessageID = "close-unapproved"
	closes := len(sessions.closes)
	if _, err := c.Handle(t.Context(), req); !errors.Is(err, control.ErrDenied) || len(sessions.closes) != closes {
		t.Fatalf("retry authority escaped into a new close: %v", err)
	}
}

func TestProjectSwitchRollsBackBindingAndEveryArchiveTogether(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"conversation-project", "document"} {
		t.Run(kind, func(t *testing.T) {
			// Each case builds its own fixture.
			t.Parallel()
			c, _, req, _ := retirementCoordinator(t)
			book := ledgerOf(t, c)
			if err := c.projects.Declare(t.Context(), []project.Project{{ID: "next", Home: project.Home{Path: t.TempDir()}}}); err != nil {
				t.Fatal(err)
			}
			second := c.store.Conversation(req.Source.ConversationID).Sessions["codex"]
			second.AgentID, second.UpstreamID = "helper", "ns_helper"
			if err := c.store.SaveSession(second); err != nil {
				t.Fatal(err)
			}
			before := c.store.Conversation(req.Source.ConversationID)
			binding, _, _ := c.projects.Binding(t.Context(), req.Source.ConversationID)
			// Constant test-owned kinds, with no operator-provided SQL input.
			trigger := `CREATE TRIGGER refuse_switch BEFORE INSERT ON bindings WHEN NEW.kind='conversation-project' BEGIN SELECT RAISE(FAIL,'test project switch write refused'); END`
			if kind == "document" {
				trigger = `CREATE TRIGGER refuse_switch BEFORE INSERT ON bindings WHEN NEW.kind='document' AND NEW.id='state' BEGIN SELECT RAISE(FAIL,'test project switch write refused'); END`
			}
			if _, err := book.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := c.commands().projectCmd(t.Context(), req, "use next"); err == nil {
				t.Fatal("refused switch reported success")
			}
			after, _, err := c.projects.Binding(t.Context(), req.Source.ConversationID)
			if err != nil || after != binding {
				t.Fatalf("refused switch committed its binding: %+v, %v", after, err)
			}
			reopened, err := state.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, c.store.Conversation(req.Source.ConversationID)) || !reflect.DeepEqual(before, reopened.Conversation(req.Source.ConversationID)) {
				t.Fatal("refused switch archived a prefix of live sessions")
			}
			if _, err := book.DB().Exec(`DROP TRIGGER refuse_switch`); err != nil {
				t.Fatal(err)
			}
			if _, err := c.commands().projectCmd(t.Context(), req, "use next"); err != nil {
				t.Fatal(err)
			}
			after, _, _ = c.projects.Binding(t.Context(), req.Source.ConversationID)
			got := c.store.Conversation(req.Source.ConversationID)
			if after.ProjectID != "next" || after.Version != binding.Version+1 || len(got.Sessions) != 0 || len(got.Archived) != 2 {
				t.Fatal("retry did not commit exactly one binding and the complete archive set")
			}
		})
	}
}

func TestCoveredHoldRefusalLeavesEveryPriorStampIntact(t *testing.T) {
	t.Parallel()
	c, _, req, _ := retirementCoordinator(t)
	var covered []task.Task
	for _, origin := range []string{"chat", "schedule"} {
		tracked, err := c.tasks.Create(task.Task{Channel: req.Source.ConversationID, Member: "codex", Origin: origin})
		if err != nil {
			t.Fatal(err)
		}
		if origin == "chat" {
			tracked, err = c.tasks.Hold(tracked.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		covered = append(covered, tracked)
	}
	checked := 0
	ctx := control.WithMutationGuard(t.Context(), func(*ledger.Tx) error {
		checked++
		if checked == 2 {
			return control.ErrDenied
		}
		return nil
	})
	if _, err := c.holdCoveredChecked(ctx, covered); !errors.Is(err, control.ErrDenied) {
		t.Fatalf("later task guard was not refused: %v", err)
	}
	reopened, err := task.OpenLedger(ledgerOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []*task.Store{c.tasks, reopened} {
		for _, before := range covered {
			after, _ := store.Get(before.ID)
			if !before.HeldAt.Equal(after.HeldAt) || !before.UpdatedAt.Equal(after.UpdatedAt) || before.State != after.State || before.ExecutionEpoch != after.ExecutionEpoch || before.Budget != after.Budget {
				t.Fatal("refused cancellation left a new hold or replaced an earlier stop")
			}
		}
	}
}

func TestModelPreferenceRefusalDoesNotChangeTheNativeModel(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"authority", "write", "native update", "ignored update", "empty readback"} {
		t.Run(failure, func(t *testing.T) {
			// Each case builds its own fixture.
			t.Parallel()
			book := testLedger(t)
			store, err := state.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			catalog, _ := agent.NewCatalog(map[string]agent.Config{"grok": {Harness: "grok", Default: true}})
			runner := &recoveryConfigurable{fakeRunner: &fakeRunner{}, settings: view.Settings{Model: "m1"}}
			if failure == "empty readback" {
				runner.settings.Model = ""
			}
			rt := &selectorRuntime{fakeManager: &fakeManager{runners: map[string]*fakeRunner{"grok": runner.fakeRunner}}, runner: runner}
			c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), rt, time.Minute, onLedger(book))
			if err := store.SetPreferences("chat", "grok", map[string]string{"model": runner.settings.Model}); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if failure == "authority" {
				ctx = control.WithMutationGuard(ctx, func(*ledger.Tx) error { return control.ErrDenied })
			} else if failure == "write" {
				if _, err := book.DB().Exec(`CREATE TRIGGER refuse_model BEFORE INSERT ON bindings WHEN NEW.kind='document' AND NEW.id='state' BEGIN SELECT RAISE(FAIL,'test preference write refused'); END`); err != nil {
					t.Fatal(err)
				}
			} else if failure == "native update" {
				runner.refused = true
			} else {
				runner.ignore = true
			}
			if _, err := c.commands().modelCmd(ctx, Request{Source: Source{ConversationID: "chat"}}, catalog.Default(), "m2"); err == nil {
				t.Fatal("refused model change reported success")
			}
			if failure == "native update" || failure == "ignored update" || failure == "empty readback" {
				if store.Preferences("chat", "grok")["model"] != "m2" {
					t.Fatal("uncertain native update lost the admitted model preference")
				}
				if _, err := handle(c, t.Context(), "try the selected model"); err == nil || len(runner.seen()) != 0 {
					t.Fatal("input reached the wrong model after native update failure")
				}
				runner.refused = false
				runner.ignore = false
				if _, err := handle(c, t.Context(), "try again"); err != nil || runner.Settings().Model != "m2" || len(runner.seen()) != 1 {
					t.Fatalf("next input did not recover the admitted model: %v", err)
				}
			} else if runner.Settings().Model != "m1" || store.Preferences("chat", "grok")["model"] != "m1" {
				t.Fatal("failed admission left the live model ahead of the durable preference")
			}
		})
	}
}
