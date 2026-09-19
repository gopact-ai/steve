package turn

import (
	"bytes"
	"reflect"
	"testing"
)

func TestTaskCompletionWithoutConsoleGuardRejectsExistingDocument(t *testing.T) {
	for _, raw := range []string{"", "{}", "null", "invalid"} {
		t.Run(raw, func(t *testing.T) {
			c, book := completionCoordinator(t, &fakeRunner{reply: "accepted"})
			if _, err := handle(c, t.Context(), "work"); err != nil {
				t.Fatal(err)
			}
			before, _ := c.tasks.Get("1")
			durableBefore, _, err := book.Document("tasks").Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := book.Document("console").Save([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := handle(c, t.Context(), "/tasks complete 1"); err == nil {
				t.Fatal("completion bypassed an unwired console owner")
			}
			if after, _ := c.tasks.Get("1"); !reflect.DeepEqual(before, after) {
				t.Fatal("refused completion changed the in-memory task")
			}
			durableAfter, _, err := book.Document("tasks").Load()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(durableBefore, durableAfter) {
				t.Fatal("refused completion changed the durable task or epoch")
			}
		})
	}
}
