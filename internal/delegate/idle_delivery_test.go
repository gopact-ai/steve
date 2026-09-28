package delegate

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// A result the parent's conversation has queued, and not yet taken up,
// is looked at on every reconciliation pass. Finding it still queued
// changes nothing, so an idle pass writes nothing: not the child's
// record, and no write transaction either.
func TestAContinuationStillQueuedIsNotRewrittenEachPass(t *testing.T) {
	w := newWorld(t)
	book := testLedger(t)
	var err error
	w.tasks, err = task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	w.service.tasks = w.tasks
	parent := w.running(t, "codex")
	child := completedChild(t, w, parent, "waiting on the owner")
	w.service.SetDeliverer(func(context.Context, Delivery) error { return channel.ErrDeliveryQueued })
	w.service.Flush(t.Context(), parent.ID)
	queued, _ := w.tasks.Get(child.ID)
	if queued.Delivery == nil || queued.Delivery.State != task.DeliveryQueued {
		t.Fatalf("delivery = %+v, want queued", queued.Delivery)
	}
	w.service.SetDeliverer(func(context.Context, Delivery) error { t.Fatal("a queued continuation was sent again"); return nil })
	w.service.SetDeliveryReceipt(func(task.Task, string) (bool, error) { return true, channel.ErrDeliveryQueued })
	writes := &countingReplicator{book: book}
	if err := book.AttachReplication(writes); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for pass := range 3 {
		w.service.reconcileDeliveries(t.Context(), now.Add(time.Duration(pass)*5*time.Second))
	}
	if writes.prepared != 0 || writes.proposed != 0 {
		t.Errorf("three idle passes opened %d write transactions and committed %d", writes.prepared, writes.proposed)
	}
	if got, _ := w.tasks.Get(child.ID); !reflect.DeepEqual(got.Delivery, queued.Delivery) {
		t.Errorf("delivery changed from %+v to %+v", queued.Delivery, got.Delivery)
	}
}

// A parent that has ended is sent nothing more. A continuation its
// conversation still holds without a settled receipt, as one waiting on
// the owner's answer does, no longer keeps the child queued: the child is
// suppressed, says why in the Hub's language, and is left alone by later
// passes, which keep the reason as it was written.
func TestAnEndedParentSuppressesAContinuationItsConversationStillHolds(t *testing.T) {
	for _, locale := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		for _, end := range []task.State{task.StateDone, task.StateCancelled} {
			t.Run(string(locale)+"/"+string(end), func(t *testing.T) {
				w := newWorld(t)
				w.service.text = i18n.New(locale)
				parent := w.running(t, "codex")
				child := completedChild(t, w, parent, "waiting on the owner")
				w.service.SetDeliverer(func(context.Context, Delivery) error { return channel.ErrDeliveryQueued })
				w.service.Flush(t.Context(), parent.ID)
				if _, err := w.tasks.Advance(parent.ID, end); err != nil {
					t.Fatal(err)
				}
				w.service.SetDeliverer(func(context.Context, Delivery) error { t.Error("an ended parent was sent a continuation"); return nil })
				lookups := 0
				w.service.SetDeliveryReceipt(func(task.Task, string) (bool, error) { lookups++; return true, channel.ErrDeliveryQueued })
				now := time.Now()
				w.service.reconcileDeliveries(t.Context(), now)
				got, _ := w.tasks.Get(child.ID)
				if got.Delivery == nil || got.Delivery.State != task.DeliverySuppressed {
					t.Fatalf("delivery = %+v, want suppressed", got.Delivery)
				}
				checkSuppressedReason(t, *got.Delivery, locale, parent.ID, end)
				settled := *got.Delivery
				w.service.text = i18n.New(otherLocale(locale))
				for pass := 1; pass <= 2; pass++ {
					w.service.reconcileDeliveries(t.Context(), now.Add(time.Duration(pass)*5*time.Second))
				}
				if lookups != 1 {
					t.Errorf("receipt looked up %d times, want once", lookups)
				}
				if got, _ := w.tasks.Get(child.ID); got.Delivery == nil || !reflect.DeepEqual(*got.Delivery, settled) {
					t.Errorf("delivery changed from %+v to %+v", settled, got.Delivery)
				}
			})
		}
	}
}

// checkSuppressedReason wants the reason a person reads beside a
// suppressed result: in the Hub's language, naming the parent and how it
// ended, and not the internal delivery key, which only the log carries.
func checkSuppressedReason(t *testing.T, d task.Delivery, locale i18n.Locale, parent string, end task.State) {
	t.Helper()
	text := i18n.New(locale)
	ended := text.T(i18n.DelegateStateDone)
	if end == task.StateCancelled {
		ended = text.T(i18n.DelegateStateCancelled)
	}
	for _, want := range []string{"#" + parent, ended} {
		if !strings.Contains(d.Error, want) {
			t.Errorf("reason %q does not name %q", d.Error, want)
		}
	}
	if d.Key != "" && strings.Contains(d.Error, d.Key) {
		t.Errorf("reason %q shows the delivery key %q", d.Error, d.Key)
	}
	if containsHan(d.Error) != (locale == i18n.LocaleZH) {
		t.Errorf("reason %q is not in %s", d.Error, locale)
	}
}

func otherLocale(locale i18n.Locale) i18n.Locale {
	if locale == i18n.LocaleEN {
		return i18n.LocaleZH
	}
	return i18n.LocaleEN
}

// countingReplicator stands in for ledger replication and counts what the
// ledger asks of it.
type countingReplicator struct {
	book               *ledger.Ledger
	prepared, proposed int
}

func (r *countingReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	r.prepared++
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}

func (r *countingReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	r.proposed++
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}
