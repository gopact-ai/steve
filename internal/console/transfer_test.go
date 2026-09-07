package console

import (
	"encoding/json"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestProjectTransferIsScopedDurableAndIdempotent(t *testing.T) {
	source := &memDoc{}
	saved := transcript{Replies: map[string][]consoleapi.Reply{"console:mixed": {{ID: "a", Conversation: "console:mixed", ProjectID: "a", Kind: "reply", Text: "project A"}, {ID: "b", Conversation: "console:mixed", ProjectID: "b", Kind: "reply", Text: "secret B"}}}, Meta: map[string]Meta{"console:mixed": {Title: "private project B title"}}, Exchanges: map[string][]*queuedExchange{"console:mixed": {{Exchange: Exchange{ID: "e-a", Conversation: "console:mixed", ExpectedProject: "a", Input: "work", Key: "client:key", State: "done"}, PayloadHash: "hash", Receipt: &consoleapi.Reply{ID: "a", ExchangeID: "e-a", Conversation: "console:mixed", ProjectID: "a", Text: "project A", Kind: "reply"}}}}, Questions: map[string]consoleapi.PendingQuestion{"q": {ID: "q", Conversation: "console:mixed", ExchangeID: "e-a", Project: "a", Principal: "owner", State: "pending"}}}
	raw, _ := json.Marshal(saved)
	_ = source.Save(raw)
	exported, err := ExportProject(source, "a", []string{"console:mixed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Replies["console:mixed"]) != 1 || len(exported.Meta) != 0 {
		t.Fatalf("foreign project content leaked: %+v", exported)
	}
	target := &memDoc{}
	if err := ImportProject(target, exported); err != nil {
		t.Fatal(err)
	}
	first, _, _ := target.Load()
	if err := ImportProject(target, exported); err != nil {
		t.Fatal(err)
	}
	second, _, _ := target.Load()
	if string(first) != string(second) {
		t.Fatal("repeated import changed data")
	}
	got, err := loadTranscript(target)
	if err != nil {
		t.Fatal(err)
	}
	if got.Questions["q"].State != "interrupted" || got.Exchanges["console:mixed"][0].PayloadHash != "hash" {
		t.Fatal("transfer lost decision/submission state")
	}
	exported.Replies["console:mixed"][0].Text = "conflict"
	if err := ImportProject(target, exported); err == nil {
		t.Fatal("conflicting import replaced content")
	}
	after, _, _ := target.Load()
	if string(after) != string(first) {
		t.Fatal("failed import partially committed")
	}
}

func TestProjectTransferRejectsUnattributableHistory(t *testing.T) {
	doc := &memDoc{}
	raw, _ := json.Marshal(transcript{Replies: map[string][]consoleapi.Reply{"console:a": {{ID: "old", Conversation: "console:a", Text: "unknown project"}}}})
	_ = doc.Save(raw)
	if _, err := ExportProject(doc, "a", []string{"console:a"}); err == nil {
		t.Fatal("old history guessed from current conversation binding")
	}
}
