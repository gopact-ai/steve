package gateway

import (
	"context"
	"strings"
	"sync"
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

type cancelingProcessor struct{}

func (cancelingProcessor) Handle(context.Context, string, string) (turn.Result, error) {
	return turn.Result{}, context.Canceled
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

func TestGatewayRepliesCanceledTurn(t *testing.T) {
	g := New(cancelingProcessor{})
	r := &reply{text: make(chan string, 1)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "long task"})

	select {
	case got := <-r.text:
		if got != "任务已取消" {
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

type blockingProcessor struct {
	mu      sync.Mutex
	seen    []string
	release chan struct{}
}

func (p *blockingProcessor) Handle(_ context.Context, _, text string) (turn.Result, error) {
	first := false
	p.mu.Lock()
	p.seen = append(p.seen, text)
	first = len(p.seen) == 1
	p.mu.Unlock()
	if first {
		<-p.release
	}
	return turn.Result{Text: "reply: " + text}, nil
}

func (p *blockingProcessor) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func TestGatewayCancelDrainsQueuedMessages(t *testing.T) {
	processor := &blockingProcessor{release: make(chan struct{})}
	g := New(processor)
	r := &reply{text: make(chan string, 4)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_1", Text: "long task"})
	deadline := time.Now().Add(time.Second)
	for len(processor.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(processor.texts()) == 0 {
		t.Fatal("worker did not pick up the first message")
	}
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_2", Text: "queued"})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_3", Text: "/cancel"})
	close(processor.release)

	deadline = time.Now().Add(time.Second)
	for len(processor.texts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for _, seen := range processor.texts() {
		if seen == "queued" {
			t.Fatal("queued message ran after cancel")
		}
	}
	if got := processor.texts(); len(got) < 2 || got[0] != "long task" || got[1] != "/cancel" {
		t.Fatalf("unexpected processing order: %v", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("short", maxReplyRunes); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
	long := strings.Repeat("长", maxReplyRunes+1)
	got := truncateRunes(long, maxReplyRunes)
	if !strings.HasPrefix(got, strings.Repeat("长", maxReplyRunes)) {
		t.Fatal("truncated text lost its prefix")
	}
	if !strings.Contains(got, "已截断") {
		t.Fatal("truncated text is missing the truncation notice")
	}
}
