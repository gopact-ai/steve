package console

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

func TestDiscardForgetsTheThreadAndSaysSo(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	s := New(&echo{}, "ou_owner", model)
	if _, err := s.Send(context.Background(), "one", "/fleet"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := s.Send(context.Background(), "two", "/fleet"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := s.Update(context.Background(), "one", consoleapi.ConversationPatch{Title: strptr("named")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	events, stop := model.Subscribe(context.Background())
	defer stop()

	release, err := s.Seal("one")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := s.Enqueue(context.Background(), "one", "hello", nil); !errors.Is(err, consoleapi.ErrBusy) {
		t.Fatalf("a sealed conversation took new work: %v", err)
	}
	if err := s.Discard("one"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	release()

	if replies := s.Replies("one"); len(replies) != 0 {
		t.Fatalf("lines survived: %+v", replies)
	}
	for _, c := range s.Summaries(context.Background()) {
		if c.ID == "console:one" {
			t.Fatalf("deleted conversation is still listed: %+v", c)
		}
	}
	if replies := s.Replies("two"); len(replies) == 0 {
		t.Fatalf("another conversation lost its lines")
	}
	if err := s.Discard("one"); err == nil {
		t.Fatalf("discarding an unknown conversation was accepted")
	}
	select {
	case event := <-events:
		if event.Kind != "console.deleted" || event.Conversation != "console:one" {
			t.Fatalf("event = %+v", event)
		}
	default:
		t.Fatal("the page was not told the conversation is gone")
	}
}

func strptr(s string) *string { return &s }
