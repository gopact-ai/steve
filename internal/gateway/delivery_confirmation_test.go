package gateway

import (
	"errors"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/turn"
	"testing"
	"time"
)

func TestDeliveryConfirmationWaitsForParentAndBindsTask(t *testing.T) {
	p := scheduledProcessor{started: make(chan turn.Request, 1), finish: make(chan struct{})}
	g := New(p)
	g.BindChannel(&scheduledNotice{id: "receipt"})
	confirmed := make(chan error, 1)
	r := Revival{TaskID: "parent", Member: "worker", ConversationID: "chat", ChatID: "chat", MessageID: "anchor", Requester: "owner", ChatType: "p2p"}
	err := g.DeliverConfirmed(r, "result ready", "continue", func(err error) { confirmed <- err })
	if !errors.Is(err, channel.ErrOutcomeUnknown) {
		t.Fatalf("in-memory dispatch reported durable success: %v", err)
	}
	req := <-p.started
	if req.ExpectedTask != r.TaskID || req.ConversationID != r.ConversationID || req.Input != "@worker continue" {
		t.Fatalf("lost binding: %+v", req)
	}
	select {
	case <-confirmed:
		t.Fatal("receipt arrived before processing")
	default:
	}
	close(p.finish)
	select {
	case err := <-confirmed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("receipt lost")
	}
}
