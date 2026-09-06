package gateway

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/turn"
)

type scheduledNotice struct {
	id    string
	err   error
	calls int
}

func (c *scheduledNotice) Reply(context.Context, string, string) error { return nil }
func (c *scheduledNotice) ReplyText(context.Context, string, string) (string, error) {
	c.calls++
	return c.id, c.err
}

type scheduledProcessor struct {
	started chan turn.Request
	finish  chan struct{}
}

func (p scheduledProcessor) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	p.started <- req
	<-p.finish
	return turn.Result{Text: "done"}, nil
}

func testFire() Fire {
	return Fire{Channel: "feishu", ProjectID: "original-project", ScheduleID: "1", ConversationID: "chat", ChatID: "oc_chat", ChatType: "p2p", MessageID: "original-anchor", Requester: "original-owner", Member: "builder", Prompt: "work"}
}

func TestScheduleReceiptWaitsForProcessingAndPreservesContext(t *testing.T) {
	p := scheduledProcessor{started: make(chan turn.Request, 1), finish: make(chan struct{})}
	g := New(p)
	notice := &scheduledNotice{id: "notice"}
	g.BindChannel(notice)
	type outcome struct {
		r   FireReceipt
		err error
	}
	done := make(chan outcome, 1)
	go func() { r, err := g.FireSchedule(t.Context(), testFire()); done <- outcome{r, err} }()
	req := <-p.started
	if req.Channel != "feishu" || req.ExpectedProject != "original-project" || req.SenderOpenID != "original-owner" || req.Input != "@builder work" || req.Origin != "schedule:1" {
		t.Fatalf("scheduled context drifted: %+v", req)
	}
	select {
	case got := <-done:
		t.Fatalf("in-memory dispatch reported durable acceptance: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	close(p.finish)
	got := <-done
	if got.err != nil || got.r.MessageID != "notice" {
		t.Fatalf("receipt=%+v err=%v", got.r, got.err)
	}
}

func TestScheduleNoticeErrorsDoNotRunAndUnknownIsExplicit(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		err      error
		unknown  bool
	}{
		{"empty", "", nil, true}, {"EOF", "", io.EOF, true}, {"cancelled", "", context.Canceled, true}, {"refused", "", errors.New("provider refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &countingProcessor{}
			g := New(p)
			g.BindChannel(&scheduledNotice{id: tc.id, err: tc.err})
			_, err := g.FireSchedule(t.Context(), testFire())
			if err == nil || errors.Is(err, channel.ErrOutcomeUnknown) != tc.unknown || p.calls.Load() != 0 {
				t.Fatalf("outcome=%v calls=%d", err, p.calls.Load())
			}
		})
	}
	p := &countingProcessor{}
	g := New(p)
	notice := &scheduledNotice{id: "notice"}
	g.BindChannel(notice)
	f := testFire()
	f.Channel = "console"
	f.ConversationID = "console:work"
	if _, err := g.FireSchedule(t.Context(), f); err == nil || notice.calls != 0 || p.calls.Load() != 0 {
		t.Fatal("console schedule reached IM")
	}
}
