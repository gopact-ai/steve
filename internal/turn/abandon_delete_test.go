package turn

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func abandonWithSession(t *testing.T) (*Coordinator, attempt.Record) {
	t.Helper()
	c, _, r := abandonFixture(t)
	if err := c.store.SaveSession(state.Session{ConversationID: "console:original", AgentID: r.Agent, HarnessID: r.Harness, NodeID: r.Node, UpstreamID: r.Session, Workspace: r.Workspace.Path}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 1); err != nil {
		t.Fatal(err)
	}
	return c, r
}
func proveAbandonedExit(t *testing.T, c *Coordinator, r attempt.Record) {
	t.Helper()
	tracked, _ := c.tasks.Get(r.TaskID)
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: r.Session, Harness: r.Harness, State: nodewire.SessionClosed, ProcessStopped: true, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: tracked.ID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: r.Execution.Epoch}}}
	if _, err := c.attempts.ConfirmTaskStopped(t.Context(), r.ID, "fixture", proof); err != nil {
		t.Fatal(err)
	}
}
func TestAbandonedConversationWaitsForExitProjectionAndOwedCloseBeforeDeletion(t *testing.T) {
	for _, phase := range []string{"unconfirmed", "unprojected", "close owed"} {
		t.Run(phase, func(t *testing.T) {
			c, r := abandonWithSession(t)
			control := NewAbandonControl(c)
			if phase != "unprojected" {
				if err := control.ProjectAbandoned(t.Context(), r.ID); err != nil {
					t.Fatal(err)
				}
			}
			if phase != "unconfirmed" {
				proveAbandonedExit(t, c, r)
				if _, err := c.attempts.MarkStopProjected(t.Context(), r.ID, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "unconfirmed" {
				for _, owed := range c.store.OwedCloses() {
					if err := c.store.SettleOwedClose(owed); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := c.store.Conversation("console:original")
			if err := c.DiscardConversation(t.Context(), "console:original"); !errors.Is(err, task.ErrRetirementPending) {
				t.Fatalf("deleted %s abandonment: %v", phase, err)
			}
			if _, found := c.tasks.Get(r.TaskID); !found {
				t.Fatal("cleanup task authority was deleted")
			}
			if !sameAbandonJSON(before, c.store.Conversation("console:original")) {
				t.Fatal("refused deletion changed session state")
			}
			if phase == "unconfirmed" {
				proveAbandonedExit(t, c, r)
				if _, err := c.attempts.MarkStopProjected(t.Context(), r.ID, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			if err := control.ProjectAbandoned(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}
			for _, owed := range c.store.OwedCloses() {
				if err := c.store.SettleOwedClose(owed); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.DiscardConversation(t.Context(), "console:original"); err != nil {
				t.Fatalf("completed cleanup still blocks deletion: %v", err)
			}
		})
	}
}
func TestConversationDeletionRechecksNewCloseAfterIdlePreflight(t *testing.T) {
	c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
	tracked, err := tasks.Create(task.Task{Channel: "console:preflight", Transport: "console", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.ChannelIdle(t.Context(), tracked.Channel, checkConversationRetirement); err != nil {
		t.Fatal(err)
	}
	session := state.Session{ConversationID: tracked.Channel, AgentID: "worker", NodeID: "node", HarnessID: "mock", UpstreamID: "ns_owed"}
	if err := c.store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	owed := state.OwedClose{NodeID: session.NodeID, HarnessID: session.HarnessID, UpstreamID: session.UpstreamID, TaskID: tracked.ID, AttemptID: "previous", OwedAt: time.Now().Format(time.RFC3339Nano)}
	if err := c.store.ArchiveSessionOwingClose(tracked.Channel, session.AgentID, owed.OwedAt, owed); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.DeleteChannel(t.Context(), tracked.Channel, checkConversationRetirement); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("new close was missed in deletion transaction: %v", err)
	}
	if _, found := tasks.Get(tracked.ID); !found {
		t.Fatal("refused delete changed task cache")
	}
}
func TestConversationDeletionFailsClosedOnUnreadableCleanupState(t *testing.T) {
	c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
	tracked, err := tasks.Create(task.Task{Channel: "console:invalid", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		return tx.PutBinding("document", "state", map[string]any{"owed_closes": []map[string]string{{"task_id": tracked.ID}}})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.DeleteChannel(t.Context(), tracked.Channel, checkConversationRetirement); err == nil {
		t.Fatal("malformed cleanup state was ignored")
	}
	if _, found := tasks.Get(tracked.ID); !found {
		t.Fatal("failed guard removed its task")
	}
}

func TestTaskDeletionRechecksAnExecutionQuarantinedAfterIdlePreflight(t *testing.T) {
	c, r := abandonWithSession(t)
	control := NewAbandonControl(c)
	if err := control.ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	proveAbandonedExit(t, c, r)
	if _, err := c.attempts.MarkStopProjected(t.Context(), r.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, owed := range c.store.OwedCloses() {
		if err := c.store.SettleOwedClose(owed); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.tasks.ChannelIdle(t.Context(), "console:original", checkConversationRetirement); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "late-observation", attempt.ErrStopConfirmationRequired, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.DeleteChannel(t.Context(), "console:original", checkConversationRetirement); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("late unresolved execution lost its authority: %v", err)
	}
	if _, found := c.tasks.Get(r.TaskID); !found {
		t.Fatal("late quarantine guard removed the task")
	}
}
