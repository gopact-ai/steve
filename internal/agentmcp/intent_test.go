package agentmcp

import (
	"context"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
)

// attemptsOf plays the attempt service: which attempt is live for a task.
type attemptsOf struct{ live string }

func (a *attemptsOf) LiveAttemptOf(context.Context, string) (string, bool) {
	return a.live, a.live != ""
}

// A send the hub never heard back about blocks the same send from the next
// attempt of the task until a person resolves it; the ledger records both.
func TestUnknownOutcomeBlocksTheSameCallFromTheNextAttempt(t *testing.T) {
	s, sender := startServer(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	intents := intent.New(book)
	attempts := &attemptsOf{live: "att-1"}
	s.SetIntents(intent.ForAgents{S: intents, Attempts: attempts})
	s.Delegated("oc_a", "codex", "t1", "", "tok-a", "")
	s.Anchor("oc_a", channel.Address{Channel: "feishu", Conversation: "oc_a", Message: "om_a"})

	// Attempt 1: the platform times out — outcome unknown.
	sender.timeout = true
	args := map[string]any{"content": "## milestone\nphase 1"}
	if text, isError := callTool(t, s.URL(), "tok-a", "channel_send", args); !isError {
		t.Fatalf("timed-out send reported success: %q", text)
	}
	unresolved, _ := intents.Unresolved(context.Background())
	if len(unresolved) != 1 || unresolved[0].AttemptID != "att-1" || unresolved[0].TaskID != "t1" {
		t.Fatalf("unresolved = %+v", unresolved)
	}
	// Attempt 2 of the same task asks for the same call: blocked, nothing sent.
	sender.timeout = false
	attempts.live = "att-2"
	text, isError := callTool(t, s.URL(), "tok-a", "channel_send", args)
	if !isError || !strings.Contains(text, "blocked pending reconciliation") || !strings.Contains(text, unresolved[0].ID) {
		t.Fatalf("second attempt's send = %q isError=%v", text, isError)
	}
	if len(sender.cards) != 0 {
		t.Fatal("a blocked call reached the platform")
	}
	// A different call is fine.
	if text, isError := callTool(t, s.URL(), "tok-a", "channel_send", map[string]any{"content": "## other\nnews"}); isError {
		t.Fatalf("a different call was blocked: %q", text)
	}
	// A person says the first one happened: the same call goes through now.
	if _, err := intents.Resolve(context.Background(), unresolved[0].ID, "happened", "owner"); err != nil {
		t.Fatal(err)
	}
	if text, isError := callTool(t, s.URL(), "tok-a", "channel_send", args); isError || !strings.Contains(text, "sent message_id=") {
		t.Fatalf("send after resolution = %q isError=%v", text, isError)
	}
	all, _ := intents.ForTask(context.Background(), "t1")
	if len(all) != 3 {
		t.Fatalf("intents on record = %d, want 3 (unknown→resolved, other, resent)", len(all))
	}
}
