package onboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/state"
)

func TestStartSendsAndRelocates(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_owner"); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saw TurnRequest
	err = Start(t.Context(), Request{
		Owner: "ou_owner",
		Home:  dir,
		Store: store,
		Handle: func(_ context.Context, req TurnRequest) (TurnResult, error) {
			saw = req
			if err := store.SaveSession(state.Session{
				ConversationID: req.ConversationID,
				AgentID:        "codex",
				HarnessID:      "codex",
				UpstreamID:     "up1",
				Workspace:      dir,
			}); err != nil {
				t.Fatal(err)
			}
			return TurnResult{Text: "你好，怎么称呼？"}, nil
		},
		Send: func(_ context.Context, id, text string) (string, error) {
			if id != "ou_owner" || text != "你好，怎么称呼？" {
				t.Fatalf("send %s %q", id, text)
			}
			return "oc_dm", nil
		},
		Catalog: i18n.New(i18n.LocaleZH),
	})
	if err != nil {
		t.Fatal(err)
	}
	if saw.ConversationID != PendingID("ou_owner") || saw.SenderOpenID != "ou_owner" || saw.ChatType != "p2p" {
		t.Fatalf("request = %#v", saw)
	}
	if !strings.Contains(saw.Input, dir) || !strings.Contains(saw.Input, "飞书") {
		t.Fatalf("prompt = %s", saw.Input)
	}
	got := store.Conversation("oc_dm")
	if got.Sessions["codex"].UpstreamID != "up1" || got.Sessions["codex"].ConversationID != "oc_dm" {
		t.Fatalf("relocated = %#v", got)
	}
	if _, ok := store.Conversation(PendingID("ou_owner")).Sessions["codex"]; ok {
		t.Fatal("pending conversation still present")
	}
	if !store.Onboarded() {
		t.Fatal("expected onboarded")
	}
}

func TestStartSkipsWhenHomeIsCustomized(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileSoul), []byte("# Soul\nSteve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileUser), []byte("# User\nLee\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := Start(t.Context(), Request{
		Owner: "ou_owner",
		Home:  dir,
		Store: store,
		Handle: func(context.Context, TurnRequest) (TurnResult, error) {
			called = true
			return TurnResult{}, nil
		},
		Send: func(context.Context, string, string) (string, error) {
			called = true
			return "", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("onboard ran after home was already filled")
	}
	if store.Onboarded() {
		t.Fatal("filled home should not mark onboarded")
	}
}

func TestStartSkipsWhenAlreadyOnboarded(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_owner"); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOnboarded(); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := Start(t.Context(), Request{
		Owner: "ou_owner",
		Home:  dir,
		Store: store,
		Handle: func(context.Context, TurnRequest) (TurnResult, error) {
			called = true
			return TurnResult{}, nil
		},
		Send: func(context.Context, string, string) (string, error) {
			called = true
			return "", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("onboard ran after it had already finished")
	}
}

func TestStartNoOwnerIsNoop(t *testing.T) {
	if err := Start(t.Context(), Request{}); err != nil {
		t.Fatal(err)
	}
}

func TestPromptEnglish(t *testing.T) {
	got := Prompt(i18n.LocaleEN, "/tmp/home")
	if !strings.Contains(got, "Feishu") || !strings.Contains(got, "/tmp/home") {
		t.Fatalf("prompt = %s", got)
	}
}
