package console

import (
	"encoding/json"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestFreezeSourceProjectKeepsOtherQueueAndStableReceipts(t *testing.T) {
	saved := transcript{Replies: map[string][]consoleapi.Reply{}, Exchanges: map[string][]*queuedExchange{"console:c": {{Exchange: Exchange{ID: "p-queue", Conversation: "console:c", ExpectedProject: "p", State: "queued", Key: "stable"}, PayloadHash: "hash"}, {Exchange: Exchange{ID: "q-queue", Conversation: "console:c", ExpectedProject: "q", State: "queued"}}}}, Questions: map[string]consoleapi.PendingQuestion{"question": {ID: "question", Project: "p", State: "pending"}}}
	raw, _ := json.Marshal(saved)
	doc := &ledger.StagedDocument{Raw: raw, Exists: true}
	if err := FreezeProject(doc, "p"); err != nil {
		t.Fatal(err)
	}
	first := string(doc.Raw)
	if err := FreezeProject(doc, "p"); err != nil {
		t.Fatal(err)
	}
	if string(doc.Raw) != first {
		t.Fatal("repeat freeze changed stable receipt")
	}
	got, err := loadTranscript(doc)
	if err != nil {
		t.Fatal(err)
	}
	p, q := got.Exchanges["console:c"][0], got.Exchanges["console:c"][1]
	if p.State != "failed" || p.Receipt == nil || p.Receipt.ID != "moved-p-queue" || p.PayloadHash != "hash" || q.State != "queued" {
		t.Fatalf("queue=%+v %+v", p, q)
	}
	if got.Questions["question"].State != "interrupted" {
		t.Fatal("old question remains dispatchable")
	}
}
