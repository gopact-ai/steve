package console

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestProjectRemapPreservesProseAndOriginalCommandIdentity(t *testing.T) {
	quotes := []QuoteRef{{Conversation: "console:source", ReplyID: "r-source"}}
	_, _, _, hash := submission("original input", "", quotes)
	r := consoleapi.Reply{ID: "r-answer", Conversation: "console:source", ProjectID: "p", ExchangeID: "e1", Text: "task #1 original prose", Process: &consoleapi.Process{Steps: []consoleapi.StepProcess{{ID: "#1", Goal: "task #1 goal"}}}}
	in := ProjectTransfer{Schema: 1, Project: "p", Conversations: []string{"console:source"}, Replies: map[string][]consoleapi.Reply{"console:source": {r}}, Exchanges: map[string][]TransferExchange{"console:source": {{Exchange: Exchange{ID: "e1", Conversation: "console:source", ExpectedProject: "p", Input: "edited after acceptance", State: "done", Key: "client:retry", Quotes: quotes}, PayloadHash: hash, Receipt: &r}}}, Questions: map[string]consoleapi.PendingQuestion{"q1": {ID: "q1", Conversation: "console:source", Project: "p", TaskID: "1", ExchangeID: "e1", State: "answered", Message: "task #1 original question"}}}
	conv := func(id string) string { return "console:hub~" + strings.TrimPrefix(id, "console:") }
	out, err := in.Remap(func(id string) string { return "hub~" + id }, conv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Replies["console:source"][0].Process.Steps[0].ID != "#1" {
		t.Fatal("remap changed source snapshot")
	}
	got := out.Replies["console:hub~source"][0]
	if got.Text != r.Text || got.Process.Steps[0].ID != "#hub~1" || got.Process.Steps[0].Goal != r.Process.Steps[0].Goal {
		t.Fatalf("remap corrupted prose or IDs: %+v", got)
	}
	if q := out.Questions["q1"]; q.TaskID != "hub~1" || q.Conversation != "console:hub~source" || q.Message != in.Questions["q1"].Message {
		t.Fatalf("question remap %+v", q)
	}
	doc := &memDoc{}
	if err := ImportProject(doc, out); err != nil {
		t.Fatal(err)
	}
	s := New(&echo{}, "owner", nil)
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	e, err := s.EnqueueCommand(context.Background(), "console:hub~source", "original input", "retry", []QuoteRef{{Conversation: "console:hub~source", ReplyID: "r-source"}})
	if err != nil || e.ID != "e1" {
		t.Fatalf("mapped retry lost original immutable input: %+v %v", e, err)
	}
	if _, err := s.EnqueueCommand(context.Background(), "console:hub~source", "different", "retry", []QuoteRef{{Conversation: "console:hub~source", ReplyID: "r-source"}}); !errors.Is(err, consoleapi.ErrCommandConflict) {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"quote_aliases"`) {
		t.Fatal("original quote namespace was not persisted")
	}
}

func TestProjectRemapMovesStructuredStepRefsInRepliesAndReceipts(t *testing.T) {
	reply := consoleapi.Reply{Conversation: "console:a", Process: &consoleapi.Process{Steps: []consoleapi.StepProcess{{ID: "#1", StepInfo: consoleapi.StepInfo{Answer: "original task 1", Refs: []string{"task 1", "git abc"}}}}}}
	in := ProjectTransfer{Replies: map[string][]consoleapi.Reply{"console:a": {reply}}, Exchanges: map[string][]TransferExchange{"console:a": {{Receipt: &reply}}}}
	out, err := in.Remap(func(id string) string { return "origin~" + id }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []consoleapi.Reply{out.Replies["console:a"][0], *out.Exchanges["console:a"][0].Receipt} {
		step := got.Process.Steps[0]
		if step.Refs[0] != "task origin~1" || step.Refs[1] != "git abc" || step.Answer != "original task 1" {
			t.Fatalf("structured ref or prose changed incorrectly: %+v", step)
		}
	}
	if in.Replies["console:a"][0].Process.Steps[0].Refs[0] != "task 1" {
		t.Fatal("source result was mutated")
	}
}
