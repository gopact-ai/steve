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
			tracked, err := tasks.Create(task.Task{Goal: "earlier", Channel: req.Source.ConversationID, Transport: req.Source.Channel, Member: "codex", ChatID: req.Reply.ChatID, ChatType: string(req.Source.ChatType)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tasks.BeginTurn(tracked.ID, "codex", "laptop", task.TurnInput{Address: req.Address(), ChatID: req.Reply.ChatID, ChatType: string(req.Source.ChatType)}); err != nil {
				t.Fatal(err)
			}
			req.Admission.ExpectedTask = tracked.ID
			_, err = c.beginTask(req, agent.Agent{ID: "codex"}, "delivery", project.Binding{}, "/w")
			var refusal UserError
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Text, "#"+tracked.ID) || !strings.Contains(refusal.Text, "/tasks cancel "+tracked.ID) {
				t.Fatalf("continuation refusal = %v", err)
			}
			if got := strings.Contains(refusal.Text, c.text.T(i18n.RecoveryRetry)); got != (req.Source.Channel == "console") {
				t.Fatalf("recovery control in %s: %q", req.Source.Channel, refusal.Text)
			}
		})
	}
}

func TestUnconfirmedWriterInThisConversationNamesItsTask(t *testing.T) {
	for _, tc := range ownerChannels {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := taskCoordinatorOn(t, &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}, withOwner("owner"), withChannelOwner("feishu", "owner"))
			req := tc.req
			if _, err := c.projects.Bind(t.Context(), req.Source.ConversationID, "codex", "owner"); err != nil {
				t.Fatal(err)
			}
			req.Input, req.Source.MessageID = "hello", "first"
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
			req.Input, req.Source.MessageID = "again", "second"
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

func TestUnconfirmedWriterNamesAnotherConversationOnTheSameChannel(t *testing.T) {
	for _, tc := range ownerChannels {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := taskCoordinatorOn(t, &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}, withOwner("owner"), withChannelOwner("feishu", "owner"))
			other := tc.req
			other.Source.ConversationID += ":other"
			if _, err := c.projects.Bind(t.Context(), other.Source.ConversationID, "codex", "owner"); err != nil {
				t.Fatal(err)
			}
			other.Input, other.Source.MessageID = "earlier", "other"
			first, err := c.Handle(t.Context(), other)
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
			req := tc.req
			if _, err := c.projects.Bind(t.Context(), req.Source.ConversationID, "codex", "owner"); err != nil {
				t.Fatal(err)
			}
			req.Input, req.Source.MessageID = "hello", "current"
			_, err = c.Handle(t.Context(), req)
			var refusal UserError
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Text, "#"+record.TaskID) || !strings.Contains(refusal.Text, other.Source.ConversationID) {
				t.Fatalf("cross-conversation refusal = %v", err)
			}
			if req.Source.Channel == "feishu" {
				if !strings.Contains(refusal.Text, "对应的飞书会话发送 /tasks cancel "+record.TaskID) || strings.Contains(refusal.Text, c.text.T(i18n.RecoveryRetry)) {
					t.Fatalf("wrong Feishu exit: %q", refusal.Text)
				}
			} else if !strings.Contains(refusal.Text, "打开该会话") {
				t.Fatalf("wrong console exit: %q", refusal.Text)
			}
		})
	}
}
