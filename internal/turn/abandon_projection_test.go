package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
)

func TestCommittedAbandonmentReplaysAfterSessionProjectionFails(t *testing.T) {
	c, tasks, r := abandonFixture(t)
	if err := c.store.SaveSession(state.Session{ConversationID: "console:original", AgentID: r.Agent, HarnessID: r.Harness, NodeID: r.Node, UpstreamID: r.Session, Workspace: r.Workspace.Path}); err != nil {
		t.Fatal(err)
	}
	control := NewAbandonControl(c)
	committed, err := control.AbandonAttempt(t.Context(), r.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := tasks.Get(r.TaskID)
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_abandon_archive BEFORE UPDATE ON bindings WHEN NEW.kind='document' AND NEW.id='state' BEGIN SELECT RAISE(ABORT,'archive refused'); END`)
	if err := control.ProjectAbandoned(t.Context(), r.ID); err == nil {
		t.Fatal("refused session projection succeeded")
	}
	current, _ := c.attempts.Get(t.Context(), r.ID)
	if current.Abandoned == nil || !current.Abandoned.At.Equal(committed.Abandoned.At) || !current.Abandoned.ProjectedAt.IsZero() || !current.Unsettled {
		t.Fatal("projection failure lost the core decision or released it")
	}
	if s := c.store.Conversation("console:original").Sessions[r.Agent]; s.UpstreamID != r.Session {
		t.Fatal("rejected archive changed live binding")
	}
	pending, err := control.PendingAbandonments(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending abandonments=%v %v", pending, err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(`DROP TRIGGER refuse_abandon_archive`); return err }); err != nil {
		t.Fatal(err)
	}
	if err := control.ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := control.ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = c.attempts.Get(t.Context(), r.ID)
	if current.Abandoned.ProjectedAt.IsZero() || !current.Unsettled || current.StopEvidence != "" {
		t.Fatal("projection lost the writer's quarantine")
	}
	if got := c.store.ArchivedSessions("console:original", r.Agent); len(got) != 1 || got[0].AbandonedAttempt != r.ID {
		t.Fatalf("archive=%+v", got)
	}
	if len(c.store.OwedCloses()) != 1 {
		t.Fatal("original close not durably owed once")
	}
	after, _ := tasks.Get(r.TaskID)
	if !sameAbandonJSON(frozen, after) {
		t.Fatal("projection charged usage again")
	}
	pending, err = control.PendingAbandonments(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("projection remains pending=%v %v", pending, err)
	}
}
