package readmodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// observationDoc controls I/O boundaries without replacing the real Model.
type observationDoc struct {
	ledger.Doc
	beforeSave func([]byte) error
	afterLoad  func()
}

func (d *observationDoc) Save(raw []byte) error {
	if d.beforeSave != nil {
		if err := d.beforeSave(raw); err != nil {
			return err
		}
	}
	return d.Doc.Save(raw)
}

func (d *observationDoc) Load() ([]byte, bool, error) {
	raw, ok, err := d.Doc.Load()
	if d.afterLoad != nil {
		d.afterLoad()
	}
	return raw, ok, err
}

func observationFile(t *testing.T) ledger.Doc {
	t.Helper()
	return &ledger.FileDocument{Path: filepath.Join(t.TempDir(), "observations.json")}
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

func restoredObservations(t *testing.T, doc ledger.Doc) []Observation {
	t.Helper()
	m := New(Sources{Observations: doc})
	if err := m.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	return m.observations
}

func TestObserveConcurrentSavesPreserveBothRecords(t *testing.T) {
	entered, block, release := observationGate()
	doc := &observationDoc{Doc: observationFile(t), beforeSave: func(raw []byte) error {
		var list []Observation
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		if len(list) == 1 {
			block()
		}
		return nil
	}}
	m := New(Sources{Observations: doc})
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
	got := restoredObservations(t, doc)
	if len(got) != 2 || !reflect.DeepEqual(got, m.observations) {
		t.Fatalf("successful observations lost on reload: live=%+v restored=%+v", m.observations, got)
	}
}

func TestObserveDocumentIODoesNotBlockReadersOrEvents(t *testing.T) {
	for _, operation := range []string{"save", "load"} {
		t.Run(operation, func(t *testing.T) {
			entered, block, release := observationGate()
			doc := &observationDoc{Doc: observationFile(t)}
			m := New(Sources{Observations: doc})
			run := func() { m.Observe("node.up", "A", "A connected", nil) }
			if operation == "save" {
				doc.beforeSave = func([]byte) error {
					block()
					return nil
				}
			} else {
				run()
				m = New(Sources{Observations: doc})
				if err := m.LoadObservations(); err != nil {
					t.Fatal(err)
				}
				doc.afterLoad = block
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
	base := observationFile(t)
	m := New(Sources{Observations: base})
	m.Observe("node.up", "A", "A connected", nil)
	entered, block, release := observationGate()
	doc := &observationDoc{Doc: base, afterLoad: block}
	m = New(Sources{Observations: doc})
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

func TestObserveSaveFailureKeepsLiveStateAndRetries(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			base := observationFile(t)
			fail := false
			doc := &observationDoc{Doc: base, beforeSave: func(raw []byte) error {
				if !fail {
					return nil
				}
				if committed {
					if err := base.Save(raw); err != nil {
						return err
					}
				}
				return errors.New("injected save failure")
			}}
			m := New(Sources{Observations: doc})
			m.Observe("node.up", "A", "A connected", nil)
			events, stop := m.Subscribe(t.Context())
			defer stop()
			fail = true
			m.Observe("node.down", "B", "B disconnected", map[string]string{"reason": "offline"})
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
			if got := restoredObservations(t, base); !reflect.DeepEqual(got, m.observations[:want]) {
				t.Fatalf("failed save changed successful durable state: %+v", got)
			}
			fail = false
			m.Observe("node.up", "C", "C connected", nil)
			if got := restoredObservations(t, base); len(got) != 3 || !reflect.DeepEqual(got, m.observations) {
				t.Fatalf("next successful save did not retain failed observation: %+v", got)
			}
		})
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
	m := New(Sources{Observations: book.Document("observations")})
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
	doc := reopened.Document("observations")
	got := restoredObservations(t, doc)
	if len(got) != count || !reflect.DeepEqual(got, m.observations) {
		t.Fatalf("ledger reopen lost successful observations: live=%d restored=%d", len(m.observations), len(got))
	}
	restored := New(Sources{Observations: doc})
	if err := restored.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	restored.Observe("node.down", "node-00", "node-00 disconnected", nil)
	if got := restoredObservations(t, doc); len(got) != count+1 || !reflect.DeepEqual(got, restored.observations) {
		t.Fatalf("saving after recovery lost observations: %+v", got)
	}
}

func TestObservePersistenceKeepsBoundedTail(t *testing.T) {
	doc := observationFile(t)
	list := make([]Observation, observationsKept)
	for i := range list {
		list[i] = Observation{At: time.Unix(int64(i+1), 0).UTC(), Kind: "node.up", Subject: fmt.Sprint(i)}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Save(raw); err != nil {
		t.Fatal(err)
	}
	m := New(Sources{Observations: doc})
	if err := m.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	m.Observe("node.up", "new", "new connected", nil)
	got := restoredObservations(t, doc)
	if len(got) != observationsKept || !reflect.DeepEqual(got, m.observations) {
		t.Fatalf("retained history differs after restart: live=%d restored=%d", len(m.observations), len(got))
	}
	if !reflect.DeepEqual(got[:len(got)-1], list[1:]) || got[len(got)-1].Subject != "new" {
		t.Fatal("retention did not discard exactly the oldest observation")
	}
}
