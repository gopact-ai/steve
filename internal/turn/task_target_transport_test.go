package turn

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskControlRequiresMatchingTransport(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, verb := range []string{"pause", "cancel", "resume", "complete", "handled", "ignore", "reopen", "show"} {
			t.Run(verb+map[bool]string{false: "/implicit", true: "/explicit"}[explicit], func(t *testing.T) {
				c, _ := completionCoordinator(t, &fakeRunner{reply: "must not run"})
				tasks := c.tasks
				tracked, err := tasks.Create(task.Task{Transport: "feishu", Channel: "console:opaque", Member: "codex", Goal: "private task"})
				if err != nil {
					t.Fatal(err)
				}
				to := task.StateRunning
				if verb == "resume" {
					to = task.StatePaused
				} else if verb == "handled" || verb == "ignore" || verb == "reopen" {
					to = task.StateFailed
				}
				if _, err := tasks.Advance(tracked.ID, to); err != nil {
					t.Fatal(err)
				}
				if verb == "reopen" {
					if _, err := tasks.Settle(tracked.ID, task.SettlementHandled); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := tasks.Get(tracked.ID)
				input := "/tasks " + verb
				if explicit {
					input += " " + tracked.ID
				}
				result, _ := c.Handle(t.Context(), Request{Channel: "console", ConversationID: tracked.Channel, Locale: "en", Input: input})
				if after, _ := tasks.Get(tracked.ID); !reflect.DeepEqual(before, after) {
					t.Errorf("cross-transport %s changed task: before=%+v after=%+v", verb, before, after)
				}
				if strings.Contains(result.Text, tracked.Goal) {
					t.Error("cross-transport command exposed task details")
				}
			})
		}
	}
}

func TestTaskControlFindsOnlyItsTransportInSharedConversation(t *testing.T) {
	for _, transport := range []string{"console", "feishu"} {
		t.Run(transport, func(t *testing.T) {
			c, _ := completionCoordinator(t, &fakeRunner{reply: "must not run"})
			if err := c.SetChannelOwner("feishu", "owner"); err != nil {
				t.Fatal(err)
			}
			other := map[string]string{"console": "feishu", "feishu": "console"}[transport]
			own, err := c.tasks.Create(task.Task{Transport: transport, Channel: "same", Member: "codex"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.tasks.Advance(own.ID, task.StateRunning); err != nil {
				t.Fatal(err)
			}
			foreign, err := c.tasks.Create(task.Task{Transport: other, Channel: "same", Member: "codex"})
			if err != nil {
				t.Fatal(err)
			}
			foreign, err = c.tasks.Advance(foreign.ID, task.StateRunning)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Handle(t.Context(), Request{Channel: transport, ConversationID: "same", Input: "/tasks pause"}); err != nil {
				t.Fatal(err)
			}
			if got, _ := c.tasks.Get(own.ID); got.State != task.StatePaused {
				t.Fatal("implicit control skipped the matching transport")
			}
			if got, _ := c.tasks.Get(foreign.ID); !reflect.DeepEqual(got, foreign) {
				t.Fatal("implicit control selected the newer foreign task")
			}
		})
	}
}
