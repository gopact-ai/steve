package readmodel

import (
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

type questionSource []consoleapi.PendingQuestion

func (q questionSource) Questions(string) []consoleapi.PendingQuestion { return q }
func TestPendingQuestionsAppearInInboxAndTaskAttention(t *testing.T) {
	m := fixture(t)
	m.SetInteractions(questionSource{{ID: "waiting", State: "pending", Conversation: "console:question", TaskID: "1", Project: "p", Message: "Choose"}, {ID: "answered", State: "answered", TaskID: "1", Message: "Old"}})
	s := m.Snapshot(t.Context())
	found := false
	for _, item := range s.Inbox {
		if item.ID == "waiting" {
			found = true
			if item.Conversation != "console:question" || len(item.Choices) != 0 {
				t.Fatal("lost response route or fabricated command")
			}
		}
		if item.ID == "answered" {
			t.Fatal("resolved question remained actionable")
		}
	}
	if !found {
		t.Fatal("pending question missing from inbox")
	}
	for _, task := range s.Tasks {
		if task.ID == "1" && (task.Attention != 1 || task.Lane != "needs_you") {
			t.Fatalf("question missing from task state: %+v", task)
		}
	}
}
