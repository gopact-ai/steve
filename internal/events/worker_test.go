package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

// fakeWorker stands in for the task service behind the Worker port.
type fakeWorker struct {
	submitted []WorkRequest
	status    WorkStatus
}

func (f *fakeWorker) SubmitWork(_ context.Context, p identity.Principal, r WorkRequest) (string, error) {
	if !p.Owner {
		return "", identity.ErrDenied
	}
	f.submitted = append(f.submitted, r)
	return "work-" + r.Key, nil
}

func (f *fakeWorker) WorkStatus(_ context.Context, _ identity.Principal, receipt string) (WorkStatus, error) {
	if !strings.HasPrefix(receipt, "work-event:") {
		return WorkStatus{}, identity.ErrDenied
	}
	return f.status, nil
}

func newWorkRoute(t *testing.T) (*Service, string) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	s := &Service{Book: book, Identity: &identity.Service{Book: book, OwnerID: "owner"}}
	if _, err := s.SaveSubscription(t.Context(), Subscription{ID: "alerts", Name: "Alerts", Enabled: true, EventType: "alert.fired", Mode: "work", Conversation: "main", Project: "ops", Instruction: "Investigate the alert"}); err != nil {
		t.Fatal(err)
	}
	var id string
	err = book.Update(t.Context(), func(tx *ledger.Tx) error {
		event, err := AppendTx(tx, Event{Source: "provider:monitor", Key: "cpu-1", Type: "alert.fired", Subject: "CPU high", At: time.Now().UTC(), Payload: json.RawMessage(`{"host":"mini"}`)})
		id = event.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, id
}

func onlyDelivery(t *testing.T, s *Service) Delivery {
	t.Helper()
	page, err := s.Deliveries(t.Context(), "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("deliveries = %#v, %v", page.Items, err)
	}
	return page.Items[0]
}

func TestPumpHandsWorkToTheWorkerOnceAndFinishesFromItsStatus(t *testing.T) {
	s, eventID := newWorkRoute(t)
	worker := &fakeWorker{status: WorkStatus{State: "running"}}
	for range 3 {
		if err := s.Pump(t.Context(), worker); err != nil {
			t.Fatal(err)
		}
	}
	d := onlyDelivery(t, s)
	if len(worker.submitted) != 1 {
		t.Fatalf("work submitted %d times, want once", len(worker.submitted))
	}
	got := worker.submitted[0]
	if got.Key != "event:"+d.ID || got.Conversation != "main" || got.Project != "ops" || got.Instruction != "Investigate the alert" || got.Causation != d.ID || !strings.Contains(string(got.Context), eventID) {
		t.Fatalf("request = %#v", got)
	}
	if d.State != StateAccepted || d.Receipt != "work-event:"+d.ID {
		t.Fatalf("running work: delivery = %s receipt %q", d.State, d.Receipt)
	}
	worker.status = WorkStatus{State: StateDone}
	if err := s.Pump(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	if d := onlyDelivery(t, s); d.State != StateDone || len(worker.submitted) != 1 {
		t.Fatalf("finished work: delivery = %s, submissions %d", d.State, len(worker.submitted))
	}
}

func TestPumpRecordsWorkThatCouldNotRun(t *testing.T) {
	s, _ := newWorkRoute(t)
	worker := &fakeWorker{status: WorkStatus{Failed: true, Detail: "model quota exhausted"}}
	for range 2 {
		if err := s.Pump(t.Context(), worker); err != nil {
			t.Fatal(err)
		}
	}
	if d := onlyDelivery(t, s); d.State != StateFailed || d.Detail != "model quota exhausted" {
		t.Fatalf("delivery = %s %q", d.State, d.Detail)
	}
}
