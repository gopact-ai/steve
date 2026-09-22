package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRecordCommandAcceptsAtomicallyAndRejectsIdentityConflict(t *testing.T) {
	l, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	raw := json.RawMessage(`{"task":"original","prompt":"continue"}`)
	if err := l.RecordCommand(t.Context(), "resume:1", "gateway-recovery", "owner", raw); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := l.RecordCommand(t.Context(), "resume:1", "gateway-recovery", "owner", raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, wrong := range []struct {
		kind, actor string
		data        json.RawMessage
	}{{"other", "owner", raw}, {"gateway-recovery", "other", raw}, {"gateway-recovery", "owner", json.RawMessage(`{"task":"other"}`)}} {
		if err := l.RecordCommand(t.Context(), "resume:1", wrong.kind, wrong.actor, wrong.data); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflicting receipt accepted: %v", err)
		}
	}
	rows, err := l.Commands(t.Context(), "gateway-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].FinishedAt == nil || rows[0].Error != "" || string(rows[0].Result) != string(raw) {
		t.Fatalf("acceptance not one completed command: %+v", rows)
	}
	called := false
	got, replay, err := l.Command(t.Context(), "resume:1", "gateway-recovery", "owner", func(_ context.Context) (json.RawMessage, error) { called = true; return nil, nil })
	if err != nil || !replay || called || string(got) != string(raw) {
		t.Fatalf("not compatible with command owner: replay=%v run=%v result=%s err=%v", replay, called, got, err)
	}
}

func TestRecordCommandFailureLeavesNoInFlightReservation(t *testing.T) {
	l, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, err = l.DB().Exec(`CREATE TRIGGER reject_command_accept BEFORE INSERT ON commands BEGIN SELECT RAISE(ABORT,'accept unavailable'); END`)
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"task":"original"}`)
	if err := l.RecordCommand(t.Context(), "resume:1", "gateway-recovery", "owner", raw); err == nil {
		t.Fatal("accept failure hidden")
	}
	rows, err := l.Commands(t.Context(), "gateway-recovery")
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed accept reserved input forever: %+v %v", rows, err)
	}
	if _, err := l.DB().Exec(`DROP TRIGGER reject_command_accept`); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordCommand(t.Context(), "resume:1", "gateway-recovery", "owner", raw); err != nil {
		t.Fatal(err)
	}
}

func TestRecordCommandCannotClearUnknownDispatch(t *testing.T) {
	l, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at) VALUES ('dispatch','gateway-dispatch','owner','2026-09-19T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordCommand(t.Context(), "dispatch", "gateway-dispatch", "owner", json.RawMessage(`{}`)); !errors.Is(err, ErrInFlight) {
		t.Fatalf("unknown dispatch rewritten as accepted: %v", err)
	}
	rows, err := l.Commands(t.Context(), "gateway-dispatch")
	if err != nil || len(rows) != 1 || rows[0].FinishedAt != nil {
		t.Fatalf("lost unknown command: %+v %v", rows, err)
	}
}
