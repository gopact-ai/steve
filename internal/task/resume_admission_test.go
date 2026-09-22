package task

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
)

func resumeOwner(t *testing.T) (*ledger.Ledger, *Store, Task) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.Create(Task{Transport: "console", Channel: "console:test", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetAside(row.ID, StatePaused); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get(row.ID)
	return book, s, row
}

func TestResumeGrantAndConsumptionAreDurableAndSingleUse(t *testing.T) {
	book, s, before := resumeOwner(t)
	a := ResumeAdmission{ID: "manual-1", TaskID: before.ID, Epoch: before.ExecutionEpoch + 1}
	input := TurnInput{Address: channel.Address{Channel: before.Transport, Conversation: before.Channel, Message: "resume-input"},
		Continuation: true, ResumeAdmission: a, TurnID: "resume-input"}
	resumed, err := s.Resume(before.ID, before.ExecutionEpoch, before.State, a)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ResumeGrant.Admission != a || resumed.ResumeGrant.Consumed {
		t.Fatalf("resume did not durably grant exactly the accepted input: %+v", resumed.ResumeGrant)
	}
	s, err = OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckResumeAdmission(a); err != nil {
		t.Fatalf("reopened grant: %v", err)
	}
	resumed, _ = s.Get(before.ID)
	for _, wrong := range []TurnInput{
		{Address: input.Address, Continuation: true, TurnID: input.TurnID},
		{Address: input.Address, Continuation: true, TurnID: input.TurnID, ResumeAdmission: ResumeAdmission{ID: "other", TaskID: before.ID, Epoch: a.Epoch}},
	} {
		if _, err := s.BeginTurn(before.ID, "worker", "", wrong); err == nil {
			t.Fatal("input without the exact grant was admitted")
		}
	}
	if _, err := s.BeginTurn(before.ID, "other-member", "", input); err == nil {
		t.Fatal("another member consumed the grant")
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER refuse_consume BEFORE UPDATE ON bindings WHEN NEW.kind='task' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(before.ID, "worker", "", input); err == nil {
		t.Fatal("SQLite write refusal was ignored")
	}
	unchanged, _ := s.Get(before.ID)
	if !reflect.DeepEqual(resumed, unchanged) {
		t.Fatal("failed charge consumed or mutated grant")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER refuse_consume`); err != nil {
		t.Fatal(err)
	}
	admitted, err := s.BeginTurn(before.ID, "worker", "", input)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted.ResumeGrant.Consumed || admitted.ResumeGrant.TurnID != input.TurnID || len(admitted.Attempts) != 1 || admitted.Budget.Turns != 1 || admitted.Attempts[0].TurnID != input.TurnID {
		t.Fatalf("consume and accounting did not commit together: %+v", admitted)
	}
	if _, err := s.Finish(before.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	s, err = OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Get(before.ID)
	if _, err := s.Resume(before.ID, before.ExecutionEpoch, before.State, a); err != nil {
		t.Fatalf("same resume identity should only replay: %v", err)
	}
	if _, err := s.BeginTurn(before.ID, "worker", "", input); !errors.Is(err, ErrResumeConsumed) {
		t.Fatalf("consumed grant created another Prompt: %v", err)
	}
	if after, _ := s.Get(before.ID); !reflect.DeepEqual(snapshot, after) {
		t.Fatal("same-key replay changed task facts")
	}
}

func TestResumeConsumptionCASRejectsAnotherOwnerStop(t *testing.T) {
	book, s, before := resumeOwner(t)
	a := ResumeAdmission{ID: "manual", TaskID: before.ID, Epoch: before.ExecutionEpoch + 1}
	if _, err := s.Resume(before.ID, before.ExecutionEpoch, before.State, a); err != nil {
		t.Fatal(err)
	}
	stale, _ := s.Get(before.ID)
	newOwner, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newOwner.SetAside(before.ID, StatePaused); err != nil {
		t.Fatal(err)
	}
	if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error { return CheckResumeAdmissionTx(tx, a) }); !errors.Is(err, ErrExecutionStopped) {
		t.Fatalf("bounded owner read ignored newer stop: %v", err)
	}
	// The old in-memory owner still sees its grant. Revision CAS must reject
	// consumption and the charge together, not overwrite newer durable facts.
	_, err = s.BeginTurn(before.ID, before.Member, "", TurnInput{
		Address: before.Address(), Continuation: true, TurnID: "input", ResumeAdmission: a,
	})
	if !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("stale owner consumed newer task authority: %v", err)
	}
	if got, _ := s.Get(before.ID); !reflect.DeepEqual(got, stale) {
		t.Fatal("failed stale-owner consumption installed task changes")
	}
	reopened, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reopened.Get(before.ID)
	if got.State != StatePaused || got.ResumeGrant.Consumed || got.Budget != before.Budget || len(got.Attempts) != 0 {
		t.Fatalf("stale consume overwrote durable stop: %+v", got)
	}
}

func TestResumeCannotReplaceOutstandingGrant(t *testing.T) {
	_, s, before := resumeOwner(t)
	first := ResumeAdmission{ID: "first", TaskID: before.ID, Epoch: before.ExecutionEpoch + 1}
	granted, err := s.Resume(before.ID, before.ExecutionEpoch, before.State, first)
	if err != nil {
		t.Fatal(err)
	}
	second := ResumeAdmission{ID: "second-click", TaskID: before.ID, Epoch: granted.ExecutionEpoch + 1}
	if _, err := s.Resume(before.ID, granted.ExecutionEpoch, granted.State, second); err == nil {
		t.Fatal("another click replaced an outstanding input's authority")
	}
	if got, _ := s.Get(before.ID); !reflect.DeepEqual(got, granted) {
		t.Fatal("refused duplicate modified outstanding grant")
	}
}

func TestResumeGrantTransferPreservesIdentityAndConsumption(t *testing.T) {
	_, s, before := resumeOwner(t)
	a := ResumeAdmission{ID: "original-input", TaskID: before.ID, Epoch: before.ExecutionEpoch + 1}
	granted, err := s.Resume(before.ID, before.ExecutionEpoch, before.State, a)
	if err != nil {
		t.Fatal(err)
	}
	in := ProjectTransfer{Tasks: map[string]*Task{before.ID: &granted}, Meta: map[string]Meta{}}
	in.Remap(ledger.TransferIDs{Namespace: "hub"})
	mapped := in.Tasks["hub~"+before.ID]
	if mapped == nil || mapped.ResumeGrant.Admission.ID != a.ID || mapped.ResumeGrant.Admission.TaskID != mapped.ID ||
		mapped.ResumeGrant.Admission.Epoch != a.Epoch || mapped.ResumeGrant.Consumed {
		t.Fatalf("remap changed grant identity: %+v", mapped)
	}
}

func TestResumeUncommittedAndRevokedGrantCannotBorrowNewEpoch(t *testing.T) {
	for _, failedWrite := range []bool{true, false} {
		t.Run(map[bool]string{true: "refused", false: "late-pause"}[failedWrite], func(t *testing.T) {
			book, s, before := resumeOwner(t)
			old := ResumeAdmission{ID: "old", TaskID: before.ID, Epoch: before.ExecutionEpoch + 1}
			if failedWrite {
				if _, err := book.DB().Exec(`CREATE TRIGGER refuse_grant BEFORE UPDATE ON bindings WHEN NEW.kind='task' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.Resume(before.ID, before.ExecutionEpoch, before.State, old)
			if (err != nil) != failedWrite {
				t.Fatalf("grant result: %v", err)
			}
			if failedWrite {
				if _, err := book.DB().Exec(`DROP TRIGGER refuse_grant`); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.SetAside(before.ID, StatePaused); err != nil {
				t.Fatal(err)
			}
			current, _ := s.Get(before.ID)
			fresh := ResumeAdmission{ID: "new", TaskID: before.ID, Epoch: current.ExecutionEpoch + 1}
			if _, err := s.Resume(before.ID, current.ExecutionEpoch, current.State, fresh); err != nil {
				t.Fatal(err)
			}
			input := TurnInput{Address: before.Address(), Continuation: true, TurnID: "old-input", ResumeAdmission: old}
			if _, err := s.BeginTurn(before.ID, before.Member, "", input); err == nil {
				t.Fatal("obsolete input borrowed a later resume")
			}
			if after, _ := s.Get(before.ID); after.Budget != before.Budget || len(after.Attempts) != len(before.Attempts) {
				t.Fatal("obsolete input charged task")
			}
		})
	}
}
