package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/turn"
)

type reply struct{ text chan string }

func (r *reply) Reply(_ context.Context, _, text string) error {
	r.text <- text
	return nil
}

type fakeProcessor struct{}

func (fakeProcessor) Handle(_ context.Context, _, text string) (turn.Result, error) {
	return turn.Result{Text: "reply: " + text}, nil
}

type countingProcessor struct{ calls atomic.Int32 }

func (p *countingProcessor) Handle(_ context.Context, _, text string) (turn.Result, error) {
	p.calls.Add(1)
	return turn.Result{Text: "reply: " + text}, nil
}

func TestGatewayReplies(t *testing.T) {
	g := New(fakeProcessor{})
	r := &reply{text: make(chan string, 1)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"})

	select {
	case got := <-r.text:
		if got != "reply: hello" {
			t.Fatalf("unexpected reply: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reply")
	}
}

func TestGatewayDeduplicatesMessageID(t *testing.T) {
	processor := &countingProcessor{}
	g := New(processor)
	r := &reply{text: make(chan string, 2)}
	g.BindChannel(r)
	msg := feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"}
	g.HandleMessage(msg)
	g.HandleMessage(msg)
	select {
	case <-r.text:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reply")
	}
	time.Sleep(20 * time.Millisecond)
	if calls := processor.calls.Load(); calls != 1 {
		t.Fatalf("processor calls = %d, want 1", calls)
	}
}
