package task

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type taskReplicator struct {
	book     *ledger.Ledger
	payloads [][]byte
	reject   error
}

func (r *taskReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}
func (r *taskReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	r.payloads = append(r.payloads, append([]byte(nil), write.Payload...))
	if r.reject != nil {
		return nil, r.reject
	}
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}
func taskRecordBook(t *testing.T) (*Store, *ledger.Ledger) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	s, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().UTC() }
	return s, book
}
func TestTaskRecordSmallWritesIgnoreTenThousandHistoricalAttempts(t *testing.T) {
	for _, sameTask := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-task=%t", sameTask), func(t *testing.T) {
			s, book := taskRecordBook(t)
			root, err := s.Create(Task{Goal: "root", Member: "owner", Channel: "chat"})
			if err != nil {
				t.Fatal(err)
			}
			next := s.clone()
			base := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)
			for i := range 10000 {
				row := Attempt{ExecutionID: fmt.Sprintf("past-%d", i), TurnID: fmt.Sprintf("past-turn-%d", i), StartedAt: base, EndedAt: base.Add(time.Second), Member: "old", Outcome: OutcomeOK}
				if sameTask {
					next.Tasks[root.ID].Attempts = append(next.Tasks[root.ID].Attempts, row)
				} else {
					id := fmt.Sprint(i + 2)
					next.Tasks[id] = &Task{ID: id, Goal: "history", State: StateDone, ExecutionEpoch: 1, Attempts: []Attempt{row}, CreatedAt: base, UpdatedAt: base}
				}
			}
			next.NextID = 10002
			if err := s.replaceLocked(next); err != nil {
				t.Fatal(err)
			}
			replicated := &taskReplicator{book: book}
			if err := book.AttachReplication(replicated); err != nil {
				t.Fatal(err)
			}
			bounded := func(name string, run func() error) {
				t.Helper()
				before := len(replicated.payloads)
				if err := run(); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				total := 0
				for _, payload := range replicated.payloads[before:] {
					total += len(payload)
				}
				t.Logf("%s payload=%d bytes", name, total)
				if total > 24<<10 {
					t.Fatalf("%s rewrote history: payload=%d bytes", name, total)
				}
			}
			bounded("begin", func() error { _, err := s.Begin(root.ID, "owner", "node", ""); return err })
			token, _ := s.ExecutionToken(root.ID)
			bounded("bind", func() error { return s.BindAttempt(token, "current", "turn") })
			bounded("meta", func() error { title := "new title"; _, err := s.SetMeta(root.ID, MetaPatch{Title: &title}); return err })
			ended := time.Now().UTC()
			bounded("settle", func() error {
				return s.SettleAttempt(root.ID, "current", "turn", ended, OutcomeOK, RecoveryUsage{Tokens: Tokens{Input: 7}, Reported: true})
			})
			before := len(replicated.payloads)
			bounded("settle-retry", func() error {
				return s.SettleAttempt(root.ID, "current", "turn", ended, OutcomeOK, RecoveryUsage{Tokens: Tokens{Input: 7}, Reported: true})
			})
			if len(replicated.payloads) != before {
				t.Fatal("identical settlement produced another mutation")
			}
			reopened, err := OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := reopened.Get(root.ID)
			if !ok || got.Budget.Tokens.Total != 7 || got.Budget.Turns != 1 || reopened.MetaOf(root.ID).Title != "new title" {
				t.Fatalf("record reopen lost state: %+v", got)
			}
			wantRows := 1
			if sameTask {
				wantRows += 10000
			}
			if len(got.Attempts) != wantRows {
				t.Fatalf("history rows=%d want=%d", len(got.Attempts), wantRows)
			}
		})
	}
}

func TestTaskRecordRejectedReplicationRollsBackMemoryAndDurableRows(t *testing.T) {
	s, book := taskRecordBook(t)
	root, err := s.Create(Task{Goal: "root", Member: "parent", Budget: Budget{MaxTurns: 2}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Spawn(root.ID, Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	before := s.List("")
	denied := errors.New("quorum rejected")
	replicated := &taskReplicator{book: book, reject: denied}
	if err := book.AttachReplication(replicated); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(child.ID, "child", "node", ""); !errors.Is(err, denied) {
		t.Fatalf("begin err=%v", err)
	}
	reopened, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, s.List("")) || !reflect.DeepEqual(before, reopened.List("")) {
		t.Fatal("failed write partially installed row or ancestor charge")
	}
	replicated.reject = nil
	if _, err := s.Begin(child.ID, "child", "node", ""); err != nil {
		t.Fatal(err)
	}
	reopened, err = OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := reopened.Get(root.ID)
	got, _ := reopened.Get(child.ID)
	if parent.Budget.Turns != 1 || got.Budget.Turns != 1 || len(got.Attempts) != 1 {
		t.Fatal("successful retry lost atomic admission")
	}
}
