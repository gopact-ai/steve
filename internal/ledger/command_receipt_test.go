package ledger

import (
	"encoding/json"
	"testing"
)

func TestCommandReceiptDistinguishesAbsentUnknownAndFinished(t *testing.T) {
	book := historyBook(t)
	if _, found, err := book.CommandReceipt(t.Context(), "input/dispatch"); err != nil || found {
		t.Fatalf("missing command found=%v err=%v", found, err)
	}
	if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at) VALUES ('input/dispatch','dispatch','owner','2026-09-19T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	receipt, found, err := book.CommandReceipt(t.Context(), "input/dispatch")
	if err != nil || !found || receipt.FinishedAt != nil || receipt.Kind != "dispatch" || receipt.Actor != "owner" {
		t.Fatalf("unknown command lost: %+v found=%v err=%v", receipt, found, err)
	}
	if err := book.RecordCommand(t.Context(), "known", "result", "owner", json.RawMessage(`{"answer":"done"}`)); err != nil {
		t.Fatal(err)
	}
	receipt, found, err = book.CommandReceipt(t.Context(), "known")
	if err != nil || !found || receipt.FinishedAt == nil || receipt.Error != "" || string(receipt.Result) != `{"answer":"done"}` {
		t.Fatalf("known command lost: %+v found=%v err=%v", receipt, found, err)
	}
}

func TestCommandReceiptDoesNotDecodeAnotherCommand(t *testing.T) {
	book := historyBook(t)
	if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at) VALUES ('broken','dispatch','owner','invalid-date')`); err != nil {
		t.Fatal(err)
	}
	if err := book.RecordCommand(t.Context(), "known", "dispatch", "owner", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := book.CommandReceipt(t.Context(), "known"); err != nil || !found {
		t.Fatalf("point read scanned another command: found=%v err=%v", found, err)
	}
	if _, _, err := book.CommandReceipt(t.Context(), "broken"); err == nil {
		t.Fatal("corruption on the target hidden")
	}
}

func TestCommandReceiptRejectsFinishedWithoutDurableOutcome(t *testing.T) {
	book := historyBook(t)
	if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at,finished_at,result)
		VALUES ('unknown','dispatch','owner','2026-09-19T00:00:00Z','2026-09-19T00:00:01Z','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := book.CommandReceipt(t.Context(), "unknown"); err == nil {
		t.Fatal("NULL error was treated as a durably successful outcome")
	}
}
