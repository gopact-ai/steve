package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
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

func TestManualResumeRetainsExpectedTask(t *testing.T) {
	processor := scheduledProcessor{started: make(chan turn.Request, 1), finish: make(chan struct{})}
	gateway := New(processor)
	gateway.BindChannel(&scheduledNotice{id: "resume-notice"})
	defer close(processor.finish)
	gateway.ResumeTask(Revival{TaskID: "closed-root", Member: "worker", ConversationID: "chat", ChatID: "chat", MessageID: "anchor", Requester: "owner", ChatType: "p2p", Manual: true}, func(string, string) error { return nil })
	select {
	case request := <-processor.started:
		if request.ExpectedTask != "closed-root" {
			t.Fatalf("resume lost task binding: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("resume was not dispatched")
	}
}

type failedContinuationProcessor struct{ failure error }

func (p failedContinuationProcessor) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, p.failure
}

func TestDeliveryConfirmationPreservesParentProcessingErrors(t *testing.T) {
	for _, failure := range []error{task.ErrContinuationUnavailable, errors.New("admitted parent failed")} {
		g := New(failedContinuationProcessor{failure: failure})
		g.BindChannel(&scheduledNotice{id: "posted-notice"})
		confirmed := make(chan error, 1)
		r := Revival{TaskID: "original-parent", Member: "worker", ConversationID: "chat", MessageID: "anchor"}
		if err := g.DeliverConfirmed(r, "result ready", "continue", func(err error) { confirmed <- err }); !errors.Is(err, channel.ErrOutcomeUnknown) {
			t.Fatalf("notice prematurely settled processing: %v", err)
		}
		select {
		case err := <-confirmed:
			if !errors.Is(err, failure) {
				t.Fatalf("processing failure acknowledged as success: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("processing receipt was lost")
		}
	}
}
