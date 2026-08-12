package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
)

type reply struct {
	text chan string
}

func (r *reply) Reply(_ context.Context, _, text string) error {
	r.text <- text
	return nil
}

func TestStatusCommandReplies(t *testing.T) {
	g := New(Config{}, nil)
	r := &reply{text: make(chan string, 1)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "/status"})

	select {
	case got := <-r.text:
		if got != "ℹ️ 无活跃会话" {
			t.Fatalf("unexpected reply: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reply")
	}
}
