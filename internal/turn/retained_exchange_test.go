package turn

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestRetainedChatsForReadOnlyTheExchangeAndKeepDuplicatesVisible(t *testing.T) {
	c, _, book, original, _ := retainedChatFixture(t)
	insert := func(id, state, kind, session string) {
		t.Helper()
		raw := fmt.Sprintf(`{"id":%q,"task_id":%q,"turn_id":"web-e1","kind":%q,"node":"node-a","agent":"worker","project":"p","session":%q,"started_at":"2026-09-01T00:00:00Z"}`, id, original.TaskID, kind, session)
		if _, err := book.DB().Exec(`INSERT INTO operations VALUES(?,'attempt',?,1,1,?,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`, id, state, raw); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(conversation, message string) []string {
		t.Helper()
		items, err := c.RetainedChatsFor(t.Context(), conversation, message)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, item := range items {
			if item.Conversation != conversation || item.MessageID != message || item.TaskID != original.TaskID {
				t.Fatalf("retained chat outside the exchange: %+v", item)
			}
			out = append(out, fmt.Sprintf("%s:%v", item.AttemptID, item.Completed))
		}
		return out
	}
	if got := ids("console:main", "web-e1"); !slices.Equal(got, []string{original.ID + ":false"}) {
		t.Fatalf("exchange executions = %v", got)
	}
	if got := ids("console:other", "web-e1"); len(got) != 0 {
		t.Fatalf("another conversation matched: %v", got)
	}
	if got := ids("console:main", "web-e2"); len(got) != 0 {
		t.Fatalf("another message matched: %v", got)
	}
	// Executions the recovery never acts on stay out; a second retained one
	// of the same exchange is returned so the caller can refuse to choose.
	insert("replaced", "superseded", "chat", "ns_replaced")
	insert("delegated", "bound", "delegate", "ns_delegated")
	insert("hub", "failed", "chat", "hub-session")
	insert("second", "bound", "chat", "ns_second")
	if got := ids("console:main", "web-e1"); !slices.Equal(got, []string{original.ID + ":false", "second:true"}) {
		t.Fatalf("exchange executions = %v", got)
	}
	if err := c.CheckRetainedChats(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`INSERT INTO operations VALUES('broken','attempt','bound',1,1,'{','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRetainedChats(t.Context()); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("unreadable history passed the check: %v", err)
	}
	if _, err := c.RetainedChatsFor(t.Context(), "console:main", "web-e1"); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("unreadable history hidden from the exchange read: %v", err)
	}
}
