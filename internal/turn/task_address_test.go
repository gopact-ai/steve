package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestAdmissionPersistsExplicitAddressWithoutMessage(t *testing.T) {
	for _, transport := range []string{"console", "feishu"} {
		for _, planned := range []bool{false, true} {
			t.Run(transport+map[bool]string{false: "/chat", true: "/plan"}[planned], func(t *testing.T) {
				c, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})
				req := Request{Channel: transport, ConversationID: "same-conversation", ChatID: "console", SenderOpenID: "native-user"}
				var tracked task.Task
				var err error
				if planned {
					tracked, err = c.openPlanTaskWithPrepared(req, "goal", "", nil)
				} else {
					var id string
					id, err = c.beginTask(req, agent.Agent{ID: "codex"}, "goal", project.Binding{}, "")
					tracked, _ = tasks.Get(id)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := channel.Address{Channel: transport, Conversation: "same-conversation"}
				if tracked.Address() != want {
					t.Fatalf("address=%+v want=%+v", tracked.Address(), want)
				}
			})
		}
	}
}
