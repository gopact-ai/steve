package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/turn"
)

type firingHandler struct{ calls atomic.Int32 }

func (h *firingHandler) Handle(context.Context, turn.Request) (turn.Result, error) {
	h.calls.Add(1)
	return turn.Result{Text: "done"}, nil
}

type firingInspector struct{}

func (firingInspector) ProjectOf(context.Context, string) string { return "p" }
func (firingInspector) Changes(context.Context, string) (*consoleapi.ChangeSummary, error) {
	return nil, nil
}

type fakeScheduleIM struct {
	calls int
	err   error
}

func (f *fakeScheduleIM) FireSchedule(context.Context, gateway.Fire) (gateway.FireReceipt, error) {
	f.calls++
	return gateway.FireReceipt{MessageID: "notice"}, f.err
}

func TestConsoleScheduleCrashGapReplaysOnlyItsDurableExchange(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	store, err := schedule.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Minute)
	job, err := store.Create(schedule.Job{Channel: "console", ProjectID: "p", ConversationID: "console:scheduled", Requester: "owner", Member: "builder", Prompt: "work", AnchorMessage: "anchor", Spec: schedule.Spec{Kind: schedule.KindOnce, At: at}})
	if err != nil {
		t.Fatal(err)
	}
	due, err := store.Due(at)
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%+v %v", due, err)
	}
	h := &firingHandler{}
	page := console.New(h, "owner", nil)
	page.SetInspector(firingInspector{})
	if err := page.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginFiring(due[0].Key); err != nil {
		t.Fatal(err)
	}
	exchange, err := page.EnqueueScheduled(t.Context(), due[0])
	if err != nil {
		t.Fatal(err)
	}
	// Restart after the receiver accepted, before the scheduler saved its receipt.
	restarted, err := schedule.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.Due(at)
	if err != nil || len(again) != 1 || again[0].Key != due[0].Key {
		t.Fatalf("lost gap recovery: %+v %v", again, err)
	}
	if err := restarted.BeginFiring(again[0].Key); err != nil {
		t.Fatal(err)
	}
	im := &fakeScheduleIM{}
	dispatchFiring(t.Context(), restarted, page, im, nil, again[0])
	deadline := time.Now().Add(2 * time.Second)
	for {
		list := page.Queue("scheduled")
		if len(list) == 1 && list[0].ID == exchange.ID && list[0].State == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scheduled exchange did not finish: %+v", list)
		}
		time.Sleep(time.Millisecond)
	}
	if im.calls != 0 || h.calls.Load() != 1 {
		t.Fatalf("route/replay duplicated work: IM=%d executions=%d", im.calls, h.calls.Load())
	}
	if _, ok := restarted.Get(job.ID); ok {
		t.Fatal("accepted one-shot was not consumed")
	}
}

func TestUnknownScheduleDeliveryIsVisibleAndNeverAutomaticallyReplayed(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	store, err := schedule.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Minute)
	job, err := store.Create(schedule.Job{Channel: "feishu", ProjectID: "p", ConversationID: "chat", Requester: "owner", Member: "builder", Prompt: "work", AnchorMessage: "anchor", Spec: schedule.Spec{Kind: schedule.KindOnce, At: at}})
	if err != nil {
		t.Fatal(err)
	}
	due, _ := store.Due(at)
	if err := store.BeginFiring(due[0].Key); err != nil {
		t.Fatal(err)
	}
	im := &fakeScheduleIM{err: errors.Join(channel.ErrOutcomeUnknown, errors.New("response lost"))}
	dispatchFiring(t.Context(), store, nil, im, nil, due[0])
	pending, ok := store.Get(job.ID)
	if !ok || pending.State != schedule.FiringUnknown || pending.Error == "" || pending.Runs != 0 {
		t.Fatalf("unknown delivery consumed or hidden: %+v", pending)
	}
	if again, _ := store.Due(at.Add(time.Minute)); len(again) != 0 || im.calls != 1 {
		t.Fatal("unknown IM delivery was retried")
	}
	if err := store.ResolveFiring(job.ID, "confirm", "owner"); err != nil {
		t.Fatal(err)
	}
}
