package admin

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
)

// boundHandler answers a console line and says which project each
// conversation works in, the way the coordinator does.
type boundHandler struct{ projects map[string]string }

func (h boundHandler) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	return turn.Result{Text: "ok"}, nil
}

func (h boundHandler) Context(_ context.Context, conversationID string) (turn.Context, error) {
	return turn.Context{Conversation: conversationID, Project: &turn.ContextProject{ID: h.projects[conversationID], Bound: true}}, nil
}

func TestRemoveProjectDeletesTheThreadsThatWorkedInIt(t *testing.T) {
	a, _ := projectAdminFixture(t)
	handler := boundHandler{projects: map[string]string{"console:doomed": "remove", "console:kept": "p"}}
	a.Console = console.New(handler, "ou_owner", readmodel.New(readmodel.Sources{}))
	for _, name := range []string{"doomed", "kept"} {
		if _, err := a.Console.Send(t.Context(), name, "hello"); err != nil {
			t.Fatalf("send %s: %v", name, err)
		}
	}

	if err := a.RemoveProject(t.Context(), "remove"); err != nil {
		t.Fatalf("remove project: %v", err)
	}

	if replies := a.Console.Replies("doomed"); len(replies) != 0 {
		t.Fatalf("the removed project's thread survived: %+v", replies)
	}
	if replies := a.Console.Replies("kept"); len(replies) == 0 {
		t.Fatalf("another project's thread was deleted")
	}
	if _, exists := a.cfg().Projects["remove"]; exists {
		t.Fatal("project declaration survived")
	}
}

func TestRemoveProjectKeepsEverythingWhenAThreadIsBusy(t *testing.T) {
	a, _ := projectAdminFixture(t)
	handler := boundHandler{projects: map[string]string{"console:busy": "remove"}}
	a.Console = console.New(handler, "ou_owner", readmodel.New(readmodel.Sources{}))
	if _, err := a.Console.Send(t.Context(), "busy", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	release, err := a.Console.Seal("busy")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	defer release()

	if err := a.RemoveProject(t.Context(), "remove"); err == nil {
		t.Fatal("a project was removed while one of its threads was busy")
	}
	if _, exists := a.cfg().Projects["remove"]; !exists {
		t.Fatal("project declaration was dropped despite the refusal")
	}
	if replies := a.Console.Replies("busy"); len(replies) == 0 {
		t.Fatal("the busy thread lost its lines")
	}
}
