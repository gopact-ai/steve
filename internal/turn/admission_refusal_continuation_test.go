package turn

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestContinuationOnAnOpenAttemptIsRefusedInWords(t *testing.T) {
	for _, tc := range ownerChannels {
		t.Run(tc.name, func(t *testing.T) {
			c, tasks, _ := taskCoordinatorOn(t, &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "unused"}}})
			req := tc.req
			tracked, err := tasks.Create(task.Task{Goal: "earlier", Channel: req.ConversationID, Transport: req.Channel, Member: "codex", ChatID: req.ChatID, ChatType: string(req.ChatType)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tasks.BeginTurn(tracked.ID, "codex", "laptop", task.TurnInput{Address: req.Address(), ChatID: req.ChatID, ChatType: string(req.ChatType)}); err != nil {
				t.Fatal(err)
			}
			req.ExpectedTask = tracked.ID
			_, err = c.beginTask(req, agent.Agent{ID: "codex"}, "delivery", project.Binding{}, "/w")
			var refusal UserError
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Text, "#"+tracked.ID) || !strings.Contains(refusal.Text, "/tasks cancel "+tracked.ID) {
				t.Fatalf("continuation refusal = %v", err)
			}
			if got := strings.Contains(refusal.Text, c.text.T(i18n.RecoveryRetry)); got != (req.Channel == "console") {
				t.Fatalf("recovery control in %s: %q", req.Channel, refusal.Text)
			}
		})
	}
}

func TestUnconfirmedWriterInThisConversationNamesItsTask(t *testing.T) {
	for _, tc := range ownerChannels {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := taskCoordinatorOn(t, &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}, withOwner("owner"), withChannelOwner("feishu", "owner"))
			req := tc.req
			if _, err := c.projects.Bind(t.Context(), req.ConversationID, "codex", "owner"); err != nil {
				t.Fatal(err)
			}
			req.Input, req.MessageID = "hello", "first"
			first, err := c.Handle(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			record, err := c.attempts.Get(t.Context(), first.Attempt)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.attempts.MarkUnsettled(t.Context(), first.Attempt, "test", harness.ErrStopUnconfirmed, nil); err != nil {
				t.Fatal(err)
			}
			req.Input, req.MessageID = "again", "second"
			_, err = c.Handle(t.Context(), req)
			var refusal UserError
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Text, "#"+record.TaskID) || !strings.Contains(refusal.Text, "/tasks cancel "+record.TaskID) {
				t.Fatalf("same conversation refusal = %v", err)
			}
			if strings.Contains(refusal.Text, "另一个会话") {
				t.Fatalf("wrong conversation: %q", refusal.Text)
			}
		})
	}
}
