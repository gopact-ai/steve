package attempt

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func insertAttemptRow(t testing.TB, s *Service, id, state, data string) {
	t.Helper()
	if _, err := s.l.DB().Exec(`INSERT INTO operations VALUES(?,'attempt',?,1,1,?,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`, id, state, data); err != nil {
		t.Fatal(err)
	}
}

func stopCandidateIDs(t *testing.T, s *Service) []string {
	t.Helper()
	records, err := s.StopCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestStopCandidatesSelectOnlyExecutionsAStopPassActsOn(t *testing.T) {
	yes, no := true, false
	base := func(id string, state State) Record {
		return Record{Spec: Spec{ID: id, TaskID: "t-" + id, Kind: KindChat, Node: "n1", Execution: &task.ExecutionToken{TaskID: "t-" + id, Epoch: 1}},
			State: state, Session: "ns_" + id, SessionSettled: &yes}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Record)
		want   bool
	}{
		{"settled bound history", func(*Record) {}, false},
		{"settled failure", func(r *Record) { r.State = Failed }, false},
		{"running", func(r *Record) { r.State = Running }, true},
		{"session settlement unknown", func(r *Record) { r.SessionSettled = nil }, true},
		{"session not settled", func(r *Record) { r.SessionSettled = &no }, true},
		{"unsettled terminal", func(r *Record) { r.Unsettled = true }, true},
		{"own task stop evidence", func(r *Record) { r.StopEvidence = "task-stop/" + r.ID }, true},
		{"another execution's stop evidence", func(r *Record) { r.StopEvidence = "task-stop/other"; r.SessionSettled = &yes }, false},
		{"process stop evidence", func(r *Record) { r.StopEvidence = "process-stop/" + r.ID }, false},
		{"superseded", func(r *Record) { r.State = Superseded; r.SessionSettled = &no }, false},
		{"hub session", func(r *Record) { r.Session = "hub-1"; r.SessionSettled = &no }, false},
		{"pending session open", func(r *Record) { r.State, r.Session, r.Unsettled = Leased, "", true }, true},
		{"live without a node session", func(r *Record) { r.State, r.Session = Running, "" }, false},
		{"no node", func(r *Record) { r.Node = ""; r.SessionSettled = &no }, false},
		{"no execution", func(r *Record) { r.Execution = nil; r.SessionSettled = &no }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newService(t)
			r := base("candidate", Bound)
			tc.mutate(&r)
			raw, _ := json.Marshal(r)
			insertAttemptRow(t, s, r.ID, string(r.State), string(raw))
			got := stopCandidateIDs(t, s)
			if (len(got) == 1) != tc.want || TaskStopOwed(r) != tc.want {
				t.Fatalf("candidates=%v needed=%v want=%v", got, TaskStopOwed(r), tc.want)
			}
		})
	}
}

// The index selects exactly the records TaskStopOwed accepts: every
// combination the stop pass acts on, and nothing it would skip.
func TestStopCandidatesMatchTheStopPassOverRandomHistory(t *testing.T) {
	s, _ := newService(t)
	random := rand.New(rand.NewSource(7))
	states := []State{Leased, Prepared, Running, BindReady, Bound, Failed, Expired, BindConflict, Superseded}
	want := map[string]bool{}
	var every []string
	for i := range 600 {
		settled := random.Intn(2) == 0
		r := Record{Spec: Spec{ID: fmt.Sprintf("att-%03d", i), TaskID: "t", Kind: KindChat}, State: states[random.Intn(len(states))], Unsettled: random.Intn(4) == 0}
		if random.Intn(4) != 0 {
			r.Node = "n1"
		}
		if random.Intn(4) != 0 {
			r.Execution = &task.ExecutionToken{TaskID: "t", Epoch: 1}
		}
		switch random.Intn(3) {
		case 0:
			r.Session = "ns_" + r.ID
		case 1:
			r.Session = "local"
		}
		if random.Intn(3) != 0 {
			r.SessionSettled = &settled
		}
		switch random.Intn(3) {
		case 0:
			r.StopEvidence = "task-stop/" + r.ID
		case 1:
			r.StopEvidence = "task-stop/att-000"
		}
		raw, _ := json.Marshal(r)
		insertAttemptRow(t, s, r.ID, string(r.State), string(raw))
		every = append(every, r.ID)
		if TaskStopOwed(r) {
			want[r.ID] = true
		}
	}
	got := stopCandidateIDs(t, s)
	var expected []string
	for _, id := range every {
		if want[id] {
			expected = append(expected, id)
		}
	}
	if !slices.Equal(got, expected) {
		t.Fatalf("stop candidates differ from the stop pass:\n got %v\nwant %v", got, expected)
	}
}

func TestStopCandidatesFailOnMalformedHistory(t *testing.T) {
	for _, raw := range []string{`{`, `[]`, `null`, `{"usage":{"input":"unknown"}}`, `{"id":"other","state":"bound"}`, `{"state":"bound"}`} {
		s, _ := newService(t)
		insertAttemptRow(t, s, "bad", "bound", raw)
		if _, err := s.StopCandidates(t.Context()); err == nil || !strings.Contains(err.Error(), "bad") {
			t.Fatalf("%s: malformed attempt hidden: %v", raw, err)
		}
	}
}

func TestStopCandidatesUseBoundedIndex(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	fixture := seedSettledHistory(t, book, 2000)
	records, err := New(book).StopCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"att-running", fixture.stopID}) {
		t.Fatalf("stop candidates = %v", ids)
	}
	diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	if plan := queryPlan(t, diagnostic, stopCandidateQuery); !strings.Contains(plan, "operations_attempt_stop_candidates") || strings.Contains(plan, "SCAN operations\n") {
		t.Fatalf("unbounded query plan:\n%s", plan)
	}
}

func queryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	return plan
}

func TestRestoredReplicaKeepsStopCandidateIndex(t *testing.T) {
	source, _ := newService(t)
	settled := false
	r := Record{Spec: Spec{ID: "candidate", Node: "n1", Execution: &task.ExecutionToken{TaskID: "t", Epoch: 1}}, State: Failed, Session: "ns_1", SessionSettled: &settled}
	raw, _ := json.Marshal(r)
	insertAttemptRow(t, source, r.ID, string(r.State), string(raw))
	if _, err := source.l.DB().Exec("DROP INDEX operations_attempt_stop_candidates"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.l.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	target, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := New(target).StopCandidates(t.Context())
	if err != nil || len(got) != 1 || got[0].ID != "candidate" {
		t.Fatalf("restored stop candidates=%+v err=%v", got, err)
	}
}
