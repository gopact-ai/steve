package turn

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
)

// seedUnconfirmedWriter leaves, in another conversation on the same
// project, an execution whose stop was never confirmed.
func seedUnconfirmedWriter(t *testing.T, c *Coordinator, req Request) {
	t.Helper()
	other := Request{Channel: "console", ConversationID: "console:other", SenderOpenID: "owner", ChatType: protocol.ChatP2P, ChatID: "console", Mentioned: true, Input: "earlier work", MessageID: "other-1"}
	if _, err := c.projects.Bind(t.Context(), other.ConversationID, "codex", "owner"); err != nil {
		t.Fatal(err)
	}
	earlier, err := c.Handle(t.Context(), other)
	if err != nil {
		t.Fatalf("earlier turn: %v", err)
	}
	if err := c.attempts.MarkUnsettled(t.Context(), earlier.Attempt, "test", harness.ErrStopUnconfirmed, nil); err != nil {
		t.Fatal(err)
	}
	if other.ConversationID == req.ConversationID {
		t.Fatal("the unconfirmed writer must be another conversation's")
	}
}

// A turn refused because its task is still in an earlier attempt says so in
// words the owner can act on: which task, how to see and cancel it, and the
// recovery choice that rechecks the original execution.
func TestTurnOnAnOpenAttemptIsRefusedInWords(t *testing.T) {
	for _, tc := range ownerChannels {
		t.Run(tc.name, func(t *testing.T) {
			c, tasks, _ := taskCoordinatorOn(t, &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "must not run"}}}, withOwner("owner"), withChannelOwner("feishu", "owner"))
			req := tc.req
			tracked, err := tasks.Create(task.Task{Goal: "earlier", Channel: req.ConversationID, Transport: req.Channel, Member: "codex", Requester: "owner", ProjectID: "codex", ChatID: req.ChatID, ChatType: string(req.ChatType)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.BeginTurn(tracked.ID, "codex", "laptop", task.TurnInput{Address: req.Address(), ChatID: req.ChatID, ChatType: string(req.ChatType)}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.projects.Bind(t.Context(), req.ConversationID, "codex", "owner"); err != nil {
				t.Fatal(err)
			}
			req.Input, req.MessageID = "hello", "m1"
			_, err = c.Handle(t.Context(), req)
			var refusal UserError
			if !errors.As(err, &refusal) {
				t.Fatalf("turn = %v (%T), want words for the owner", err, err)
			}
			for _, want := range []string{"#" + tracked.ID, string(protocol.CommandTasks) + " cancel " + tracked.ID, "「" + c.text.T(i18n.RecoveryRetry) + "」"} {
				if !strings.Contains(refusal.Text, want) {
					t.Errorf("refusal %q does not say %q", refusal.Text, want)
				}
			}
			if strings.Contains(refusal.Text, "admit turn") || strings.Contains(refusal.Text, "open attempt") {
				t.Errorf("refusal %q is the internal error", refusal.Text)
			}
		})
	}
}

// A turn refused because an earlier execution in the same workspace has not
// been confirmed stopped says so in words the owner can act on.
func TestTurnBehindAnUnconfirmedWriterIsRefusedInWords(t *testing.T) {
	for _, tc := range ownerChannels {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{reply: "earlier answer"}
			c, _, _ := taskCoordinatorOn(t, &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}, withOwner("owner"), withChannelOwner("feishu", "owner"))
			req := tc.req
			seedUnconfirmedWriter(t, c, req)
			seen := len(runner.seen())
			if _, err := c.projects.Bind(t.Context(), req.ConversationID, "codex", "owner"); err != nil {
				t.Fatal(err)
			}
			req.Input, req.MessageID = "hello", "m1"
			_, err := c.Handle(t.Context(), req)
			var refusal UserError
			if !errors.As(err, &refusal) {
				t.Fatalf("turn = %v (%T), want words for the owner", err, err)
			}
			if got := len(runner.seen()); got != seen {
				t.Fatalf("the refused turn reached the agent: %v", runner.seen())
			}
			for _, want := range []string{string(protocol.CommandTasks), "「" + c.text.T(i18n.RecoveryRetry) + "」"} {
				if !strings.Contains(refusal.Text, want) {
					t.Errorf("refusal %q does not say %q", refusal.Text, want)
				}
			}
			if strings.Contains(refusal.Text, "open attempt") || strings.Contains(refusal.Text, "stop confirmation") || strings.Contains(refusal.Text, "may still be writing") {
				t.Errorf("refusal %q is the internal error", refusal.Text)
			}
		})
	}
}
