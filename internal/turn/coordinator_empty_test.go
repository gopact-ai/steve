package turn

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/state"
)

func TestNoAgentRequestExplainsRegistrationWithoutCreatingSession(t *testing.T) {
	for _, tc := range []struct {
		locale, want string
	}{
		{"zh", "还没有注册 Agent。请先在资源中注册本机 Agent，再开始任务。"},
		{"en", "No Agent is registered yet. Register a local Agent in Resources before starting a task."},
	} {
		t.Run(tc.locale, func(t *testing.T) {
			catalog, err := agent.NewCatalog(nil)
			if err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			manager := &fakeManager{}
			coordinator := New(catalog, store, nil, manager, time.Minute)
			for _, input := range []string{"hello", "@codex hello"} {
				_, err := coordinator.Handle(t.Context(), Request{Locale: tc.locale, ConversationID: "chat", Input: input})
				var userError UserError
				if !errors.As(err, &userError) || userError.Text != tc.want {
					t.Fatalf("unregistered Agent request = %v", err)
				}
				if got := store.Conversation("chat"); got.ActiveAgent != "" || len(got.Sessions) > 0 || len(manager.opened) > 0 {
					t.Fatalf("unregistered Agent request created execution state: %+v", got)
				}
			}
		})
	}
}
