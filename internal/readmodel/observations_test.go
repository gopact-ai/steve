package readmodel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// observationHooks controls I/O boundaries without replacing the real Model.
type observationHooks struct {
	ObservationStore
	beforeSave func(first uint64, list []Observation) error
	beforeLoad func() error
	afterLoad  func()
}

func (h *observationHooks) Save(ctx context.Context, first uint64, list []Observation, keep uint64) error {
	if h.beforeSave != nil {
		if err := h.beforeSave(first, list); err != nil {
			return err
		}
	}
	return h.ObservationStore.Save(ctx, first, list, keep)
}

func (h *observationHooks) Load(ctx context.Context) ([]Observation, uint64, error) {
	if h.beforeLoad != nil {
		if err := h.beforeLoad(); err != nil {
			return nil, 0, err
		}
	}
	list, last, err := h.ObservationStore.Load(ctx)
	if h.afterLoad != nil {
		h.afterLoad()
	}
	return list, last, err
}

func observationBook(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return book
}

// ledgerObservationStore is where a hub keeps observations in its ledger.
func ledgerObservationStore(book *ledger.Ledger) ObservationStore {
	return Observations{Book: book}
}

func observationStore(t *testing.T) ObservationStore {
	t.Helper()
	return ledgerObservationStore(observationBook(t))
}

func observationGate() (entered <-chan struct{}, block, release func()) {
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	return started, func() {
		close(started)
		<-resume
	}, func() { once.Do(func() { close(resume) }) }
}

func waitObservation(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observation operation did not finish")
	}
}

func restoredObservations(t *testing.T, store ObservationStore) []Observation {
	t.Helper()
	m := New(Sources{Observations: store})
	if err := m.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	return m.observations
}

func subjects(list []Observation) []string {
	out := make([]string, len(list))
	for i, o := range list {
		out[i] = o.Subject
	}
	return out
}

// An Observe that arrives while another's save is in progress waits for
// it, so both are kept, and in the order they were observed.
func TestObserveWaitsForTheSaveInProgressAndKeepsBothInOrder(t *testing.T) {
	entered, block, release := observationGate()
	store := &observationHooks{ObservationStore: observationStore(t), beforeSave: func(first uint64, _ []Observation) error {
		if first == 1 {
			block()
		}
		return nil
	}}
	m := New(Sources{Observations: store})
	first, second := make(chan struct{}), make(chan struct{})
	var workers sync.WaitGroup
	defer func() {
		release()
		workers.Wait()
	}()
	workers.Go(func() {
		m.Observe("node.up", "A", "A connected", map[string]string{"build": "a"})
		close(first)
	})
	waitObservation(t, entered)
	workers.Go(func() {
		m.Observe("node.up", "B", "B connected", map[string]string{"build": "b"})
		close(second)
	})
	// Let an unsynchronized newer write finish first. A serialized writer
	// instead waits for release; the timeout only bounds that negative check.
	select {
	case <-second:
	case <-time.After(100 * time.Millisecond):
	}
	release()
	waitObservation(t, first)
	waitObservation(t, second)
	got := restoredObservations(t, store)
	if len(got) != 2 || !reflect.DeepEqual(got, m.observations) {
		t.Fatalf("successful observations lost on reload: live=%+v restored=%+v", m.observations, got)
	}
}

func TestObserveStoreIODoesNotBlockReadersOrEvents(t *testing.T) {
	for _, operation := range []string{"save", "load"} {
		t.Run(operation, func(t *testing.T) {
			entered, block, release := observationGate()
			store := &observationHooks{ObservationStore: observationStore(t)}
			m := New(Sources{Observations: store})
			run := func() { m.Observe("node.up", "A", "A connected", nil) }
			if operation == "save" {
				store.beforeSave = func(uint64, []Observation) error {
					block()
					return nil
				}
			} else {
				run()
				m = New(Sources{Observations: store})
				if err := m.LoadObservations(); err != nil {
					t.Fatal(err)
				}
				store.afterLoad = block
				run = func() {
					if err := m.LoadObservations(); err != nil {
						t.Errorf("load observations: %v", err)
					}
				}
			}
			var workers sync.WaitGroup
			defer func() {
				release()
				workers.Wait()
			}()
			workers.Go(run)
			waitObservation(t, entered)
			read := make(chan struct{})
			workers.Go(func() {
				defer close(read)
				m.Snapshot(t.Context())
				history, _, err := m.History(t.Context(), "", 10)
				if err != nil || len(history) != 1 || history[0].Subject != "A" {
					t.Errorf("live observation during I/O: history=%+v err=%v", history, err)
				}
				events, stop := m.Subscribe(t.Context())
				defer stop()
				m.Publish(Event{Kind: "independent", Text: "still responsive"})
				if ev := <-events; ev.Kind != "independent" {
					t.Errorf("event during I/O = %+v", ev)
				}
				if recent := m.Recent(); len(recent) != 1 || recent[0].Kind != "independent" {
					t.Errorf("recent events during I/O = %+v", recent)
				}
				stop()
				if _, ok := <-events; ok {
					t.Error("subscription did not close during I/O")
				}
			})
			waitObservation(t, read)
		})
	}
}

func TestLoadObservationsDoesNotOverwriteConcurrentObserve(t *testing.T) {
	base := observationStore(t)
	m := New(Sources{Observations: base})
	m.Observe("node.up", "A", "A connected", nil)
	entered, block, release := observationGate()
	store := &observationHooks{ObservationStore: base, afterLoad: block}
	m = New(Sources{Observations: store})
	loaded, observed := make(chan struct{}), make(chan struct{})
	var workers sync.WaitGroup
	defer func() {
		release()
		workers.Wait()
	}()
	workers.Go(func() {
		defer close(loaded)
		if err := m.LoadObservations(); err != nil {
			t.Errorf("load observations: %v", err)
		}
	})
	waitObservation(t, entered)
	workers.Go(func() {
		m.Observe("node.up", "B", "B connected", nil)
		close(observed)
	})
	select {
	case <-observed:
	case <-time.After(100 * time.Millisecond):
	}
	release()
	waitObservation(t, loaded)
	waitObservation(t, observed)
	got := restoredObservations(t, base)
	if len(got) != 2 || !reflect.DeepEqual(got, m.observations) {
		t.Fatalf("concurrent restore lost an observation: live=%+v restored=%+v", m.observations, got)
	}
}

// A save that fails, whether or not the ledger committed it, keeps the fact
// live and published; the next save writes it again with its own.
func TestObserveSaveFailureKeepsLiveStateAndRetries(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			book, r := replicatedBook(t, t.TempDir())
			store := ledgerObservationStore(book)
			m := New(Sources{Observations: store})
			m.Observe("node.up", "A", "A connected", nil)
			events, stop := m.Subscribe(t.Context())
			defer stop()
			injected := func() error { return errors.New("injected save failure") }
			if committed {
				r.after = injected
			} else {
				r.before = injected
			}
			m.Observe("node.down", "B", "B disconnected", map[string]string{"reason": "offline"})
			r.before, r.after = nil, nil
			if len(m.observations) != 2 {
				t.Fatalf("failed save discarded live observation: %+v", m.observations)
			}
			select {
			case ev := <-events:
				if ev.Kind != "observe.node.down" || ev.Text != "B" || ev.Data["reason"] != "offline" {
					t.Fatalf("failed save changed the live event: %+v", ev)
				}
			default:
				t.Fatal("failed save suppressed the live event")
			}
			want := 1
			if committed {
				want = 2
			}
			if got := restoredObservations(t, store); !reflect.DeepEqual(got, m.observations[:want]) {
				t.Fatalf("failed save changed successful durable state: %+v", got)
			}
			m.Observe("node.up", "C", "C connected", nil)
			if got := restoredObservations(t, store); len(got) != 3 || !reflect.DeepEqual(got, m.observations) {
				t.Fatalf("next successful save did not retain failed observation: %+v", got)
			}
		})
	}
}

// A save the ledger committed but reported as failed leaves its facts
// unsaved in memory. Loading again finds them in the ledger already: they
// are shown once and are not written a second time under new numbers.
func TestLoadObservationsAfterAnUnacknowledgedSaveKeepsItOnce(t *testing.T) {
	book, r := replicatedBook(t, t.TempDir())
	store := ledgerObservationStore(book)
	m := New(Sources{Observations: store})
	m.Observe("node.up", "A", "A connected", nil)
	r.after = func() error { return errors.New("outcome unknown") }
	m.Observe("node.up", "B", "B connected", nil)
	r.after = nil
	if err := m.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	if live := subjects(m.observations); !reflect.DeepEqual(live, []string{"A", "B"}) {
		t.Fatalf("reload showed the committed observation again: %v", live)
	}
	m.Observe("node.up", "C", "C connected", nil)
	if got, live := subjects(restoredObservations(t, store)), subjects(m.observations); !reflect.DeepEqual(got, []string{"A", "B", "C"}) || !reflect.DeepEqual(got, live) {
		t.Fatalf("after reload: restored=%v live=%v, want A B C once each", got, live)
	}
}

// A record the hub cannot read, or one under an id it never writes, costs
// that record and not every later save: the rest load in order, numbering
// goes on past the unreadable one, and each skip is logged with its id.
func TestLoadObservationsSkipsRecordsItCannotRead(t *testing.T) {
	book := observationBook(t)
	store := ledgerObservationStore(book)
	earlier := New(Sources{Observations: store})
	for _, name := range []string{"A", "B", "C"} {
		earlier.Observe("node.up", name, name+" connected", nil)
	}
	if err := book.PutBinding(t.Context(), observationKind, observationID(2), "not an observation"); err != nil {
		t.Fatal(err)
	}
	if err := book.PutBinding(t.Context(), observationKind, "legacy", Observation{Kind: "node.up", Subject: "X"}); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	defer slog.SetDefault(previous)
	m := New(Sources{Observations: store})
	if err := m.LoadObservations(); err != nil {
		t.Fatalf("one unreadable record failed the whole load: %v", err)
	}
	if live := subjects(m.observations); !reflect.DeepEqual(live, []string{"A", "C"}) {
		t.Fatalf("loaded %v, want the readable A and C in order", live)
	}
	m.Observe("node.up", "D", "D connected", nil)
	var saved Observation
	if ok, err := book.GetBinding(t.Context(), observationKind, observationID(4), &saved); err != nil || !ok || saved.Subject != "D" {
		t.Fatalf("D kept as number 4: %t, %v, %+v; want it after C's 3", ok, err, saved)
	}
	for _, id := range []string{observationID(2), "legacy"} {
		if !strings.Contains(logged.String(), `"id":"`+id+`"`) {
			t.Errorf("skipping record %s was not logged with its id: %s", id, logged.String())
		}
	}
}

// A hub that could not read its kept observations must not number new ones
// from the start: they would be filed among, or over, the old. They wait in
// memory and are saved after the old ones once a load succeeds.
func TestObserveBeforeAFailedLoadIsSavedAfterTheKeptObservations(t *testing.T) {
	base := observationStore(t)
	earlier := New(Sources{Observations: base})
	earlier.Observe("node.up", "A", "A connected", nil)
	earlier.Observe("node.up", "B", "B connected", nil)
	failures := 2
	store := &observationHooks{ObservationStore: base, beforeLoad: func() error {
		if failures > 0 {
			failures--
			return errors.New("injected load failure")
		}
		return nil
	}}
	m := New(Sources{Observations: store})
	if err := m.LoadObservations(); err == nil {
		t.Fatal("injected load failure was not reported")
	}
	m.Observe("node.down", "C", "C disconnected", nil)
	if got := subjects(restoredObservations(t, base)); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("an observation made before the kept ones loaded was saved among them: %v", got)
	}
	m.Observe("node.up", "D", "D connected", nil)
	if got, live := subjects(restoredObservations(t, base)), subjects(m.observations); !reflect.DeepEqual(got, []string{"A", "B", "C", "D"}) || !reflect.DeepEqual(got, live) {
		t.Fatalf("after a load worked: restored=%v live=%v", got, live)
	}
}

func TestObserveConcurrentLedgerSavesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if book != nil {
			_ = book.Close()
		}
	})
	m := New(Sources{Observations: ledgerObservationStore(book)})
	const count = 32
	start := make(chan struct{})
	var writers sync.WaitGroup
	for i := range count {
		writers.Go(func() {
			<-start
			subject := fmt.Sprintf("node-%02d", i)
			m.Observe("node.up", subject, subject+" connected", map[string]string{"build": subject})
		})
	}
	close(start)
	writers.Wait()
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book = nil
	reopened, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	store := ledgerObservationStore(reopened)
	got := restoredObservations(t, store)
	if len(got) != count || !reflect.DeepEqual(got, m.observations) {
		t.Fatalf("ledger reopen lost successful observations or their order: live=%v restored=%v", subjects(m.observations), subjects(got))
	}
	restored := New(Sources{Observations: store})
	if err := restored.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	restored.Observe("node.down", "node-00", "node-00 disconnected", nil)
	if got := restoredObservations(t, store); len(got) != count+1 || !reflect.DeepEqual(got, restored.observations) {
		t.Fatalf("saving after recovery lost observations: %+v", got)
	}
}

// Past the limit every save forgets exactly the oldest, in the ledger as in
// memory, and a restart restores the same tail in order.
func TestObservePersistenceKeepsBoundedTail(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := New(Sources{Observations: ledgerObservationStore(book)})
	const extra = 5
	for i := range observationsKept + extra {
		observeNodeUp(m, i)
	}
	kept, err := book.Bindings(t.Context(), observationKind)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != observationsKept {
		t.Fatalf("ledger keeps %d observations, want %d", len(kept), observationsKept)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got := restoredObservations(t, ledgerObservationStore(reopened))
	if len(got) != observationsKept || !reflect.DeepEqual(got, m.observations) {
		t.Fatalf("retained history differs after restart: live=%d restored=%d", len(m.observations), len(got))
	}
	if first, last := got[0].Subject, got[len(got)-1].Subject; first != fmt.Sprintf("node-%04d", extra) || last != fmt.Sprintf("node-%04d", observationsKept+extra-1) {
		t.Fatalf("retention kept %s…%s, want exactly the newest %d", first, last, observationsKept)
	}
}
