package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/protocol"
)

func TestRegisteredProviderTurnKeepsNativeIdentityAndExecutionReceipt(t *testing.T) {
	runner := &fakeRunner{reply: "done"}
	c, tasks := taskCoordinator(t, runner, withOwner("console-owner"), withChannelOwner("mail", "mail-account"))
	var admittedTask, admittedAttempt string
	req := Request{
		Source:      Source{Channel: "mail", ConversationID: "opaque-thread", MessageID: "provider-message", ChatType: protocol.ChatP2P, Origin: "provider:mail"},
		Actor:       Actor{ID: "mail-account"},
		Reply:       ReplyContext{ChatID: "reply-context", CardID: "delivery-artifact"},
		Admission:   Admission{ExpectedProject: "codex"},
		Input:       "summarize this message",
		OnTurnReady: func(taskID, attemptID string) { admittedTask, admittedAttempt = taskID, attemptID },
	}
	result, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	rows := tasks.List("opaque-thread")
	if len(rows) != 1 {
		t.Fatalf("provider input did not create one task: %+v", rows)
	}
	tracked := rows[0]
	wantAddress := channel.Address{Channel: "mail", Conversation: "opaque-thread", Message: "provider-message"}
	if tracked.Address() != wantAddress || tracked.Requester != "mail-account" || tracked.Origin != "provider:mail" || tracked.ChatID != "reply-context" || tracked.OpenCard != "delivery-artifact" {
		t.Fatalf("provider identity or delivery context changed during admission: %+v", tracked)
	}
	if admittedTask != tracked.ID || admittedAttempt == "" || admittedAttempt != result.Attempt {
		t.Fatalf("observer was not bound to the execution receipt: task=%s attempt=%s result=%+v", admittedTask, admittedAttempt, result)
	}
	record, err := c.attempts.Get(t.Context(), admittedAttempt)
	if err != nil || record.By != "mail-account" || record.TurnID != "provider-message" {
		t.Fatalf("execution lost its original actor or source: %+v err=%v", record, err)
	}
	if c.ownerOpenID != "console-owner" {
		t.Fatal("provider input replaced Console's owner")
	}
}

func TestProviderSourceAndReplyMetadataCannotGrantConsoleQueueAuthority(t *testing.T) {
	c, _ := taskCoordinator(t, &fakeRunner{reply: "must not run"}, withOwner("console-owner"), withChannelOwner("mail", "console-owner"))
	proved, err := c.ConfirmNeverAdmitted(t.Context(), Request{
		Source:    Source{Channel: "mail", ConversationID: "console:spoofed", MessageID: "web-exchange"},
		Actor:     Actor{ID: "console-owner"},
		Reply:     ReplyContext{ChatID: "console", CardID: "exchange"},
		Admission: Admission{ExchangeID: "exchange"},
	})
	if err == nil || proved {
		t.Fatalf("provider metadata gained Console recovery proof: proved=%t err=%v", proved, err)
	}
}
