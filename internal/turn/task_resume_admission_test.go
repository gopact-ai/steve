package turn

import (
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskResumeDoesNotReplaceAnOutstandingGrantOnAnotherClick(t *testing.T) {
	accepted := 0
	c, _ := completionCoordinator(t, &fakeRunner{reply: "must not run"}, withCallbacks(func(cb *Callbacks) {
		cb.Resumer = func(TaskResume) error { accepted++; return nil }
	}))
	row, err := c.tasks.Create(task.Task{Transport: "console", Channel: "console:resume", Member: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.SetAside(row.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"click-1", "click-2"} {
		if _, err := c.Handle(t.Context(), Request{Channel: row.Transport, ConversationID: row.Channel, MessageID: id, Input: "/tasks resume " + row.ID}); err != nil {
			t.Fatal(err)
		}
		if id == "click-1" {
			row, _ = c.tasks.Get(row.ID)
		}
	}
	after, _ := c.tasks.Get(row.ID)
	if accepted != 1 || !reflect.DeepEqual(row, after) {
		t.Fatalf("second click replaced accepted input authority: accepted=%d before=%+v after=%+v", accepted, row, after)
	}
}
