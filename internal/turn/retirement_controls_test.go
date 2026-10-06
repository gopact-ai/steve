package turn

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
)

func TestRetirementAdmissionRejectsAlreadyParsedSessionControls(t *testing.T) {
	for _, command := range []string{"new", "history", "project", "rotation"} {
		t.Run(command, func(t *testing.T) {
			c, sessions, req, taskID := retirementCoordinator(t)
			current := c.store.Conversation(req.ConversationID).Sessions["codex"]
			archived := current
			archived.UpstreamID = "ns_archived"
			if err := c.store.SaveSession(archived); err != nil {
				t.Fatal(err)
			}
			if err := c.store.ArchiveSession(req.ConversationID, "codex", time.Now().UTC().Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			if err := c.store.SaveSession(current); err != nil {
				t.Fatal(err)
			}
			gate := &retirementGateRuntime{nodeSessions: sessions, entered: make(chan struct{}), release: make(chan struct{})}
			c.runtime = gate
			closed := make(chan error, 1)
			go func() { closed <- c.ResetConversationSessions(t.Context(), req.ConversationID) }()
			select {
			case <-gate.entered:
			case <-time.After(waitDeadline):
				t.Fatal("no close")
			}
			before := c.store.Conversation(req.ConversationID)
			beforeTask, _ := c.tasks.Get(taskID)
			selected := agent.Agent{ID: "codex", Harness: "codex", Node: "node-b"}
			switch command {
			case "new":
				if _, err := c.commands().reset(t.Context(), req, selected); err == nil {
					t.Error("parsed reset entered retirement")
				}
			case "history":
				if _, err := c.commands().historyCmd(req, selected, "1"); err == nil {
					t.Error("parsed history restore entered retirement")
				}
			case "project":
				if reply, err := c.commands().projectCmd(t.Context(), req, "use codex"); err == nil && reply.Text == "" {
					t.Error("parsed project switch entered retirement")
				}
			case "rotation":
				c.RotateTask(req.ConversationID, "codex", beforeTask.Origin)
			}
			if !reflect.DeepEqual(before, c.store.Conversation(req.ConversationID)) {
				t.Error("control changed a retiring slot")
			}
			got, exists := c.tasks.Get(taskID)
			if !exists || !reflect.DeepEqual(beforeTask, got) {
				t.Error("control changed retirement authority")
			}
			close(gate.release)
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(waitDeadline):
				t.Fatal("close stayed blocked")
			}
		})
	}
}

func TestScheduledRotationCannotCloseDuringSessionRetirement(t *testing.T) {
	sessions := &nodeSessions{fakeManager: &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}}
	c, _, _ := owedCloseCoordinator(t, sessions)
	req := ownerChannels[0].req
	req.Origin = "schedule:test"
	_, first := startOwedSession(t, c, req, "ns_owed")
	record, err := c.attempts.Get(t.Context(), first.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := c.tasks.Get(record.TaskID)
	if before.Origin != req.Origin {
		t.Fatal("invalid rotation prerequisite")
	}
	gate := &retirementGateRuntime{nodeSessions: sessions, entered: make(chan struct{}), release: make(chan struct{})}
	c.runtime = gate
	result := make(chan error, 1)
	go func() { result <- c.ResetConversationSessions(t.Context(), req.ConversationID) }()
	select {
	case <-gate.entered:
	case <-time.After(waitDeadline):
		t.Fatal("no close")
	}
	c.RotateTask(req.ConversationID, "codex", req.Origin)
	after, _ := c.tasks.Get(record.TaskID)
	if !reflect.DeepEqual(before, after) {
		t.Error("rotation changed original task/epoch during close")
	}
	close(gate.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestRetirementWaitsForAdmittedHistoryMutation(t *testing.T) {
	c, _, req, _ := retirementCoordinator(t)
	release, err := c.beginSessionRetirement(t.Context(), req.ConversationID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := c.ResetConversationSessions(t.Context(), req.ConversationID); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("retirement bypassed admitted mutation: %v", err)
	}
}
