package attempt

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// settledHistoryFixture is a fixed amount of recovery work beside n settled
// chat executions, each on its own task with a read revision. Reads that
// serve the fixed work must not grow with the settled history.
type settledHistoryFixture struct {
	stopID, turnID, delegateTask string
}

func seedSettledHistory(tb testing.TB, l *ledger.Ledger, n int) settledHistoryFixture {
	tb.Helper()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	summary := strings.Repeat("summary text ", 40)
	settled, unsettled := true, false
	record := func(i int, id, taskID string) Record {
		return Record{Spec: Spec{ID: id, TaskID: taskID, TurnID: "turn-" + id, Kind: KindChat, Project: "p", Node: "n1", Harness: "codex", Agent: "a",
			Execution: &task.ExecutionToken{TaskID: taskID, Epoch: 1},
			Workspace: project.Workspace{ID: "ws", Node: "n1", Path: "/work/p"}, Touches: []string{"a.go", "b.go"}},
			State: Bound, Revision: 5, Session: "ns_" + id, SessionSettled: &settled, Leases: []ledger.Lease{{Key: "ws:/work/p", Holder: id, Epoch: 3, ExpiresAt: at}},
			Result: &Result{Summary: summary, Artifact: "art-" + id, Refs: []string{"r1", "r2"}},
			Usage:  &Usage{Model: "m", Input: 1000, Output: 200, Reported: true}, StartedAt: at.Add(time.Duration(i) * time.Second), EndedAt: at.Add(time.Duration(i)*time.Second + time.Minute)}
	}
	fixture := settledHistoryFixture{stopID: "att-stop", turnID: "turn-att-turn", delegateTask: "task-delegate"}
	stop := record(n, fixture.stopID, "task-stop")
	stop.SessionSettled = &unsettled
	turn := record(n+1, "att-turn", "task-turn")
	delegated := record(n+2, "att-delegate", fixture.delegateTask)
	delegated.Kind = KindDelegate
	running := record(n+3, "att-running", "task-running")
	running.State, running.SessionSettled, running.Result, running.EndedAt = Running, &unsettled, nil, time.Time{}
	err := l.Update(context.Background(), func(tx *ledger.Tx) error {
		write := func(i int, r Record) error {
			raw, err := json.Marshal(r)
			if err != nil {
				return err
			}
			ts := at.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
			if _, err := tx.Exec(`INSERT INTO operations(id, kind, state, revision, incarnation, data, created_at, updated_at) VALUES (?, 'attempt', ?, 5, 1, ?, ?, ?)`, r.ID, string(r.State), string(raw), ts, ts); err != nil {
				return err
			}
			var nonce [32]byte
			copy(nonce[:], r.ID)
			return tx.PutBinding(historyRevisionKind, r.TaskID, hex.EncodeToString(nonce[:]))
		}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("att-%07d", i)
			if err := write(i, record(i, id, fmt.Sprintf("task-%07d", i))); err != nil {
				return err
			}
		}
		for i, r := range []Record{stop, turn, delegated, running} {
			if err := write(n+i, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return fixture
}

// BenchmarkSettledHistoryReads measures each recovery and usage read against
// a fixed amount of work while the settled history grows.
func BenchmarkSettledHistoryReads(b *testing.B) {
	for _, read := range []struct {
		name string
		run  func(context.Context, *Service, settledHistoryFixture) error
	}{
		{"application-stop", func(ctx context.Context, s *Service, _ settledHistoryFixture) error {
			_, err := s.StopCandidates(ctx)
			return err
		}},
		{"retained-chat", liveAndClosed},
		{"delegate-recovery", liveAndClosed},
		{"usage", liveAndClosed},
	} {
		for _, n := range []int{1000, 10000, 50000} {
			b.Run(fmt.Sprintf("%s/%d", read.name, n), func(b *testing.B) {
				l, err := ledger.Open(b.TempDir(), ledger.Options{})
				if err != nil {
					b.Fatal(err)
				}
				defer l.Close()
				fixture := seedSettledHistory(b, l, n)
				s := New(l)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := read.run(b.Context(), s, fixture); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func liveAndClosed(ctx context.Context, s *Service, _ settledHistoryFixture) error {
	if _, err := s.Live(ctx); err != nil {
		return err
	}
	_, err := s.Closed(ctx)
	return err
}
