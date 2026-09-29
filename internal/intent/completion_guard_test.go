package intent

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCompletionGuardUsesIntentOperationState(t *testing.T) {
	for _, state := range []State{Claimed, Dispatched, Unknown, Succeeded, Failed, "future"} {
		t.Run(string(state), func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { book.Close() })
			// Payload state is deliberately stale; the operation is authoritative.
			if _, err := book.Begin(t.Context(), "effect", kind, string(Succeeded), "test", Intent{TaskID: "child", State: Failed}); err != nil {
				t.Fatal(err)
			}
			if _, err := book.Begin(t.Context(), "unrelated", kind, string(Unknown), "test", Intent{TaskID: "other"}); err != nil {
				t.Fatal(err)
			}
			rollback := errors.New("rollback test transition")
			err = book.Update(t.Context(), func(tx *ledger.Tx) error {
				if _, err := tx.Exec(`UPDATE operations SET state = ? WHERE id = 'effect'`, state); err != nil {
					return err
				}
				err := CheckTaskCompletionTx(tx, map[string]bool{"root": true, "child": true})
				if state == Succeeded || state == Failed {
					if err != nil {
						t.Errorf("terminal effect: %v", err)
					}
				} else if !errors.Is(err, task.ErrCompleteAttention) {
					t.Errorf("pending effect admitted: %v", err)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
		})
	}
}

func TestCompletionGuardRejectsCorruptIntentEvenIfUnrelatedOrTerminal(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	if _, err := book.Begin(t.Context(), "effect", kind, string(Succeeded), "test", Intent{TaskID: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`UPDATE operations SET data = '{' WHERE id = 'effect'`); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return CheckTaskCompletionTx(tx, map[string]bool{"root": true})
	}); err == nil {
		t.Fatal("unreadable intent admitted completion")
	}
}

func TestCompletionGuardRejectsIntentWithCorruptEnvelope(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	if _, err := book.DB().Exec(`INSERT INTO operations VALUES('effect','intent','succeeded',1,1,'{"id":"effect","task_id":"other","state":"succeeded"}','not a time','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return CheckTaskCompletionTx(tx, map[string]bool{"root": true})
	}); err == nil {
		t.Fatal("intent with a corrupt envelope admitted completion")
	}
}

// The guard reads the intents of the tasks it is asked about, not every
// intent there is: what it costs does not grow with other tasks' effects.
func TestCompletionGuardReadsOnlyTheTasksIntents(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	if _, err := book.Begin(t.Context(), "effect", kind, string(Succeeded), "test", Intent{ID: "effect", TaskID: "root", State: Succeeded}); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
			return CheckTaskCompletionTx(tx, map[string]bool{"root": true})
		}); err != nil {
			t.Fatal(err)
		}
	}
	before := testing.AllocsPerRun(3, check)
	if _, err := book.DB().Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<10000)
		INSERT INTO operations SELECT 'history-'||n,'intent','succeeded',1,1,
		json_object('id','history-'||n,'task_id','other-'||n,'tool','steve_send','state','succeeded','at','2026-09-01T00:00:00Z'),
		'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z' FROM seq`); err != nil {
		t.Fatal(err)
	}
	after := testing.AllocsPerRun(3, check)
	t.Logf("completion guard allocations: %.0f -> %.0f", before, after)
	if after > before+30 {
		t.Fatalf("other tasks' intents are read for the guard: %.0f -> %.0f allocations", before, after)
	}
}
