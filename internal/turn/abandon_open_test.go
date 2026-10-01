package turn

import (
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func importedAbandonFixture(t *testing.T, present bool) (*Coordinator, attempt.Record, state.Session) {
	t.Helper()
	c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
	path := t.TempDir()
	ref := nativehistory.Reference{ID: "import_" + strings.Repeat("a", 64), Harness: "mock", NativeID: "native-source", SourceHome: "/source", SourceWorkdir: path, Revision: "r1", Digest: "snapshot-1", ImportedAt: time.Now().UTC()}
	binding := state.Session{ConversationID: "console:imported", AgentID: "worker", HarnessID: "mock", NodeID: "node", Workspace: path, ProjectID: "p", NativeImport: ref.Clone()}
	if present {
		if err := c.store.InstallNativeSession(binding); err != nil {
			t.Fatal(err)
		}
	}
	tracked, err := tasks.Create(task.Task{Channel: binding.ConversationID, Transport: "console", AnchorMessage: "web-original", Member: "worker", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(tracked.ID)
	r, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "open-without-receipt", TaskID: tracked.ID, TurnID: "web-original", Execution: &token, Kind: attempt.KindChat, Node: "node", Harness: "mock", Agent: "worker", Project: "p", NativeImport: ref.Clone(), Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "original", Project: "p", Node: "node", Path: path, Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.Advance(t.Context(), r.ID, attempt.Prepared, "fixture", nil); err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "fixture", attempt.ErrStopConfirmationRequired, nil); err != nil {
		t.Fatal(err)
	}
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	r, err = c.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven")
	if err != nil {
		t.Fatal(err)
	}
	return c, r, binding
}

func TestAbandonLostOpenRetiresItsPreinstalledImportedContext(t *testing.T) {
	c, r, original := importedAbandonFixture(t, true)
	control := NewAbandonControl(c)
	if _, err := control.AbandonAttempt(t.Context(), r.ID, "owner", 1); err != nil {
		t.Fatal(err)
	}
	if err := control.ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, live := c.store.Conversation(original.ConversationID).Sessions[original.AgentID]; live {
		t.Fatal("lost-open abandonment retained the original imported context")
	}
	if len(c.store.OwedCloses()) != 0 {
		t.Fatal("lost-open abandonment invented a native close identity")
	}
	if err := c.store.SaveSession(original); err == nil {
		t.Fatal("late import slot save resurrected abandoned context")
	}
	reopened, err := state.OpenLedger(ledgerOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.InstallNativeSession(original); err == nil {
		t.Fatal("original import command was resurrected after restart")
	}
	other := original
	other.NativeImport = original.NativeImport.Clone()
	other.NativeImport.ID = "import_" + strings.Repeat("b", 64)
	other.NativeImport.Digest = "different-snapshot"
	if err := reopened.SaveSession(other); err != nil {
		t.Fatalf("another explicitly selected context was blocked: %v", err)
	}
}

func TestAbandonLostOpenDoesNotRetireANewEmptySlot(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "originally absent", true: "replaced after decision"}[present], func(t *testing.T) {
			c, r, original := importedAbandonFixture(t, present)
			control := NewAbandonControl(c)
			if _, err := control.AbandonAttempt(t.Context(), r.ID, "owner", 1); err != nil {
				t.Fatal(err)
			}
			other := original
			other.NativeImport = original.NativeImport.Clone()
			other.NativeImport.ID = "import_" + strings.Repeat("c", 64)
			other.NativeImport.Digest = "new-snapshot"
			if err := c.store.SaveSession(other); err != nil {
				t.Fatal(err)
			}
			if err := control.ProjectAbandoned(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}
			got := c.store.Conversation(other.ConversationID).Sessions[other.AgentID]
			if !sameAbandonJSON(got, other) {
				t.Fatal("old abandonment captured a newer empty session slot")
			}
			if len(c.store.OwedCloses()) != 0 {
				t.Fatal("empty-slot projection invented a native close")
			}
			if err := c.store.SaveSession(original); err == nil {
				t.Fatal("original import was reusable after retirement of its missing or replaced slot")
			}
		})
	}
}

func TestAbandonLostOpenSourceCaptureRollsBackWithTheCoreDecision(t *testing.T) {
	c, r, original := importedAbandonFixture(t, true)
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_core BEFORE UPDATE ON operations WHEN NEW.id='open-without-receipt' AND json_extract(NEW.data,'$.abandoned') IS NOT NULL BEGIN SELECT RAISE(ABORT,'core refused'); END`)
	if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 1); err == nil {
		t.Fatal("rejected abandonment accepted an import retirement")
	}
	if !sameAbandonJSON(c.store.Conversation(original.ConversationID).Sessions[original.AgentID], original) {
		t.Fatal("core refusal retired the imported slot")
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(`DROP TRIGGER refuse_core`); return err }); err != nil {
		t.Fatal(err)
	}
}
