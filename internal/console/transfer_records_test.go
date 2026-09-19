package console

import (
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestRecordTransferPreservesRecoveryObligationsAndRemapsTargets(t *testing.T) {
	source, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	state := DurableState{Exchanges: map[string][]DurableExchange{"console:source": {{
		Exchange:            Exchange{ID: "queued", Conversation: "console:source", ExpectedProject: "p", ExpectedTask: "task", State: consoleapi.ExchangeQueued},
		RecoveryPending:     true,
		RecoveryStopTarget:  &recoveryStopTarget{Conversation: "console:source", ExchangeID: "original", TaskID: "task", Requester: "owner"},
		RecoveryStop:        &consoleapi.Reply{ID: "stop", Conversation: "console:source", ProjectID: "p", Text: "stable stop evidence"},
		RecoveryStopPending: "waiting for original task",
	}}}}
	storeConsoleState(t, source, state)
	exported, err := ExportProject(source, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	exported, err = exported.Remap(func(id string) string { return "target-" + id }, func(string) string { return "console:target" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.Update(t.Context(), func(tx *ledger.Tx) error { return ImportProjectTx(tx, exported) }); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(target)
	if err != nil {
		t.Fatal(err)
	}
	e := got.Exchanges["console:target"][0]
	if !e.RecoveryPending || e.RecoveryStopTarget == nil || e.RecoveryStopTarget.Conversation != "console:target" ||
		e.RecoveryStopTarget.TaskID != "target-task" || e.RecoveryStopTarget.ExchangeID != "original" ||
		e.RecoveryStop == nil || e.RecoveryStop.Conversation != "console:target" || e.RecoveryStop.Text != "stable stop evidence" ||
		e.RecoveryStopPending != "waiting for original task" {
		t.Fatalf("transfer lost or failed to remap recovery obligations: %+v", e)
	}
}
