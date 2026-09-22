package turn

import (
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskCompletionWithoutConsoleGuardRejectsExistingRecords(t *testing.T) {
	for _, raw := range []string{"", "{}", "null", "invalid"} {
		t.Run(raw, func(t *testing.T) {
			c, book := completionCoordinator(t, &fakeRunner{reply: "accepted"})
			if _, err := handle(c, t.Context(), "work"); err != nil {
				t.Fatal(err)
			}
			before, _ := c.tasks.Get("1")
			beforeStore, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			durableBefore, found := beforeStore.Get("1")
			if !found || len(durableBefore.Attempts) == 0 {
				t.Fatal("completion fixture lacks durable task accounting")
			}
			if err := func() error {
				_, err := book.DB().Exec(`INSERT INTO bindings(kind,id,data,updated_at) VALUES('console-store','state',?,'now')`, raw)
				return err
			}(); err != nil {
				t.Fatal(err)
			}
			if _, err := handle(c, t.Context(), "/tasks complete 1"); err == nil {
				t.Fatal("completion bypassed an unwired console owner")
			}
			if after, _ := c.tasks.Get("1"); !reflect.DeepEqual(before, after) {
				t.Fatal("refused completion changed the in-memory task")
			}
			afterStore, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			durableAfter, found := afterStore.Get("1")
			if !found || !reflect.DeepEqual(durableBefore, durableAfter) {
				t.Fatal("refused completion changed the durable task or epoch")
			}
		})
	}
}
