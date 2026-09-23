package attempt

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// putUsageRow writes one attempt row and sets its task's read revision, as
// the owner's save boundary does, unless revision is empty.
func putUsageRow(t *testing.T, l *ledger.Ledger, r Record, second int, revision string) {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(second) * time.Second).Format(time.RFC3339Nano)
	if err := l.Update(t.Context(), func(tx *ledger.Tx) error {
		if _, err := tx.Exec(`INSERT INTO operations(id,kind,state,revision,incarnation,data,created_at,updated_at) VALUES(?,'attempt',?,1,1,?,?,?)
			ON CONFLICT(id) DO UPDATE SET state=excluded.state,data=excluded.data,updated_at=excluded.updated_at`, r.ID, string(r.State), string(raw), at, at); err != nil {
			return err
		}
		if revision == "" {
			return nil
		}
		return tx.PutBinding(historyRevisionKind, r.TaskID, revision)
	}); err != nil {
		t.Fatal(err)
	}
}

func usageRevision(seed string) string {
	var nonce [32]byte
	copy(nonce[:], seed)
	return hex.EncodeToString(nonce[:])
}

// wantUsageSamples is every terminal attempt, decoded in full, most recently
// updated first.
func wantUsageSamples(t *testing.T, l *ledger.Ledger) []UsageSample {
	t.Helper()
	rows, err := l.DB().Query(`SELECT id,state,revision,data FROM operations WHERE kind='attempt' ORDER BY updated_at DESC,id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []UsageSample
	for rows.Next() {
		r, err := scanIdentityRecord(rows)
		if err != nil {
			t.Fatal(err)
		}
		if r.State.Terminal() {
			out = append(out, r.UsageSample())
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertUsageSamples(t *testing.T, s *Service, want []UsageSample) {
	t.Helper()
	got, err := s.UsageSamples(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage samples:\n got %+v\nwant %+v", got, want)
	}
}

func usageRecord(rng *rand.Rand, id, taskID string, at time.Time) Record {
	states := []State{Leased, Prepared, Running, Bound, Failed, Expired, Superseded}
	r := Record{Spec: Spec{ID: id, TaskID: taskID, Kind: KindChat, Project: fmt.Sprintf("p%d", rng.Intn(3)), Harness: fmt.Sprintf("h%d", rng.Intn(2)), Agent: fmt.Sprintf("a%d", rng.Intn(3)), Touches: []string{"x.go"}},
		State: states[rng.Intn(len(states))], Session: "ns_" + id, Result: &Result{Summary: "done"}, StartedAt: at, Error: "e"}
	if rng.Intn(3) > 0 {
		r.Usage = &Usage{Model: fmt.Sprintf("m%d", rng.Intn(2)), Input: rng.Int63n(1000), Output: rng.Int63n(100), CachedRead: rng.Int63n(10), CachedWrite: rng.Int63n(10), Context: rng.Int63n(5000), Reported: rng.Intn(2) == 0}
	}
	if rng.Intn(4) > 0 {
		r.EndedAt = at.Add(time.Duration(rng.Intn(600)) * time.Second)
	}
	return r
}

// Usage reads every settled attempt through its task's read revision. As
// history changes, only the tasks whose revision moved are read again, and
// the result stays what a full decode of the settled history gives.
func TestUsageSamplesFollowSettledHistoryThroughReadRevisions(t *testing.T) {
	s, _ := newService(t)
	rng := rand.New(rand.NewSource(7))
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	second, nonce := 0, 0
	records := map[string]Record{}
	put := func(r Record) {
		second++
		nonce++
		records[r.ID] = r
		putUsageRow(t, s.l, r, second, usageRevision(fmt.Sprintf("n%d", nonce)))
	}
	for i := 0; i < 60; i++ {
		put(usageRecord(rng, fmt.Sprintf("a%03d", i), fmt.Sprintf("t%d", rng.Intn(8)), base.Add(time.Duration(i)*time.Minute)))
	}
	assertUsageSamples(t, s, wantUsageSamples(t, s.l))
	for round := 0; round < 40; round++ {
		switch rng.Intn(4) {
		case 0:
			id := fmt.Sprintf("b%03d", round)
			put(usageRecord(rng, id, fmt.Sprintf("t%d", rng.Intn(10)), base.Add(time.Duration(round)*time.Hour)))
		case 1:
			r := records[fmt.Sprintf("a%03d", rng.Intn(60))]
			r.State, r.EndedAt = Failed, r.StartedAt.Add(time.Minute)
			r.Usage = &Usage{Model: "late", Input: 5, Output: 1, Reported: true}
			put(r)
		case 2:
			r := records[fmt.Sprintf("a%03d", rng.Intn(60))]
			from := r.TaskID
			r.TaskID = fmt.Sprintf("t%d", rng.Intn(10))
			put(r)
			second++
			nonce++
			if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
				return tx.PutBinding(historyRevisionKind, from, usageRevision(fmt.Sprintf("n%d", nonce)))
			}); err != nil {
				t.Fatal(err)
			}
		case 3:
			task := fmt.Sprintf("t%d", rng.Intn(10))
			if _, err := s.l.DB().Exec(`DELETE FROM operations WHERE kind='attempt' AND `+identityTask+`=?`, task); err != nil {
				t.Fatal(err)
			}
			if _, err := s.l.DB().Exec(`DELETE FROM bindings WHERE kind=? AND id=?`, historyRevisionKind, task); err != nil {
				t.Fatal(err)
			}
		}
		assertUsageSamples(t, s, wantUsageSamples(t, s.l))
	}
}

// A task whose read revision did not move is not read again.
func TestUsageSamplesRereadOnlyTasksWhoseRevisionMoved(t *testing.T) {
	s, _ := newService(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	kept := Record{Spec: Spec{ID: "kept", TaskID: "t1", Agent: "a"}, State: Bound, Usage: &Usage{Input: 1, Reported: true}, StartedAt: at}
	moved := Record{Spec: Spec{ID: "moved", TaskID: "t2", Agent: "a"}, State: Bound, Usage: &Usage{Input: 2, Reported: true}, StartedAt: at}
	putUsageRow(t, s.l, kept, 1, usageRevision("r1"))
	putUsageRow(t, s.l, moved, 2, usageRevision("r2"))
	first := wantUsageSamples(t, s.l)
	assertUsageSamples(t, s, first)
	kept.Usage.Input, moved.Usage.Input = 10, 20
	putUsageRow(t, s.l, kept, 1, "")
	putUsageRow(t, s.l, moved, 2, usageRevision("r3"))
	got, err := s.UsageSamples(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Usage.Input != 20 || got[1].Usage.Input != 1 {
		t.Fatalf("usage samples = %+v", got)
	}
}

// Every read fails while any attempt row cannot be decoded in full, even
// when usage does not need the broken field or its task is unchanged.
func TestUsageSamplesFailOnMalformedHistory(t *testing.T) {
	for name, data := range map[string]string{
		"syntax":      `{`,
		"null":        `null`,
		"field type":  `{"id":"broken","task_id":"t1","leases":"x"}`,
		"identity":    `{"id":"other","task_id":"t1"}`,
		"no revision": `{"id":"broken","task_id":"orphan","agent":"a"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newService(t)
			putUsageRow(t, s.l, Record{Spec: Spec{ID: "ok", TaskID: "t1"}, State: Bound}, 1, usageRevision("r1"))
			if _, err := s.UsageSamples(t.Context()); err != nil {
				t.Fatal(err)
			}
			insertAttemptRow(t, s, "broken", string(Bound), data)
			if name == "no revision" {
				s = New(s.l)
			}
			if got, err := s.UsageSamples(t.Context()); err == nil {
				t.Fatalf("malformed history read as %+v", got)
			}
		})
	}
}

// A restored replica may carry a revision the cache has already seen next to
// different history. Restoring discards everything read before it.
func TestUsageSamplesRereadAllHistoryAfterRestore(t *testing.T) {
	source, _ := newService(t)
	target, _ := newService(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	revision := usageRevision("same")
	putUsageRow(t, target.l, Record{Spec: Spec{ID: "a1", TaskID: "t1", Agent: "before"}, State: Bound, StartedAt: at}, 1, revision)
	assertUsageSamples(t, target, wantUsageSamples(t, target.l))
	putUsageRow(t, source.l, Record{Spec: Spec{ID: "a1", TaskID: "t1", Agent: "after"}, State: Failed, StartedAt: at}, 1, revision)
	snapshot, err := source.l.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	if err := target.l.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	want := wantUsageSamples(t, target.l)
	if len(want) != 1 || want[0].Agent != "after" {
		t.Fatalf("restored history = %+v", want)
	}
	assertUsageSamples(t, target, want)
}

func TestUsageSamplesReadChangedTasksThroughTheTaskIndex(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	fixture := seedSettledHistory(t, book, 2000)
	if _, err := New(book).UsageSamples(t.Context()); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	if plan := queryPlan(t, diagnostic, usageTasksSQL, `["`+fixture.delegateTask+`"]`); !strings.Contains(plan, "operations_attempt_task") || strings.Contains(plan, "SCAN operations\n") {
		t.Fatalf("unbounded query plan:\n%s", plan)
	}
}
