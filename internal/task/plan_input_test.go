package task

import (
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestPlanInputRemainsTheCreationInputAcrossTurnsAndTransfer(t *testing.T) {
	s, _ := newStore(t)
	tracked, err := s.Create(Task{Origin: "plan", ProjectID: "p", Transport: "console", Channel: "console:plan", AnchorMessage: "web-original", PlanMessageID: "untrusted", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if tracked.PlanMessageID != "web-original" {
		t.Fatalf("plan creation input=%q", tracked.PlanMessageID)
	}
	if _, err := s.BeginTurn(tracked.ID, "worker", "node", TurnInput{Address: channel.Address{Channel: "console", Conversation: tracked.Channel, Message: "web-newer"}, TurnID: "web-newer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(tracked.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAside(tracked.ID, StatePaused); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Get(tracked.ID)
	if _, err := s.Resume(tracked.ID, current.ExecutionEpoch, StatePaused, ResumeAdmission{}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenLedger(s.book)
	if err != nil {
		t.Fatal(err)
	}
	current, _ = reloaded.Get(tracked.ID)
	if current.PlanMessageID != "web-original" || current.AnchorMessage != "web-newer" {
		t.Fatalf("plan origin changed across turn/resume/reopen: %+v", current)
	}
	var exported ProjectTransfer
	if err := s.book.Update(t.Context(), func(tx *ledger.Tx) error { var err error; exported, err = ExportProjectTx(tx, "p"); return err }); err != nil {
		t.Fatal(err)
	}
	exported.Remap(ledger.TransferIDs{Tasks: map[string]string{tracked.ID: "101"}, Conversations: map[string]string{tracked.Channel: "console:moved"}})
	target, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	if err := target.Update(t.Context(), func(tx *ledger.Tx) error { return ImportProjectTx(tx, exported) }); err != nil {
		t.Fatal(err)
	}
	loaded, err := OpenLedger(target)
	if err != nil {
		t.Fatal(err)
	}
	moved, _ := loaded.Get("101")
	if moved.PlanMessageID != "web-original" || moved.Channel != "console:moved" {
		t.Fatalf("transfer changed plan input: %+v", moved)
	}
}

func TestOnlyAnAnchoredPlanRecordsItsCreationInput(t *testing.T) {
	s, _ := newStore(t)
	for _, row := range []Task{{Origin: "plan"}, {Origin: "chat", AnchorMessage: "web-chat", PlanMessageID: "must-not-carry"}} {
		got, err := s.Create(row)
		if err != nil {
			t.Fatal(err)
		}
		if got.PlanMessageID != "" {
			t.Fatalf("invented original plan input: %q", got.PlanMessageID)
		}
	}
}
