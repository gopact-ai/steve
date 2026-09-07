package task

import (
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestRecoveryWorkspacePreservesKnownUsageAndTaskOrigin(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := s.Create(Task{Goal: "scheduled task", Channel: "console:main", Member: "worker", Origin: "nightly", ProjectID: "p", Requester: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Begin(tracked.ID, "worker", "node-a", ""); err != nil {
		t.Fatal(err)
	}
	token, err := s.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	workspace := RecoveryWorkspace{ID: "schedule-workspace", ProjectID: "p", NodeID: "node-b", Path: "/scheduled-recovery", Base: "snapshot", AgentID: "worker", HarnessID: "test", AttemptID: "scheduled-attempt", PlanID: "scheduled-plan"}
	usage := RecoveryUsage{Tokens: Tokens{Input: 100, Output: 50, Total: 150}, Model: "known-model", Reported: true}
	for range 2 {
		if err := s.BindRecoveryWorkspace(token, workspace, usage); err != nil {
			t.Fatal(err)
		}
	}
	saved, _ := s.Get(tracked.ID)
	if saved.Budget.Turns != 1 || saved.Budget.Tokens.Total != 150 || len(saved.Attempts) != 2 || saved.Attempts[0].Tokens.Total != 150 || saved.Attempts[0].Model != "known-model" || saved.Attempts[0].UsageKnown == nil || !*saved.Attempts[0].UsageKnown {
		t.Fatalf("source spend lost or duplicated: %+v", saved)
	}
	if _, found := s.RecoveryOn(tracked.Channel, "worker", ""); found {
		t.Fatal("interactive task borrowed scheduled recovery location")
	}
	if recovered, found := s.RecoveryOn(tracked.Channel, "worker", "nightly"); !found || recovered.ID != tracked.ID {
		t.Fatal("scheduled origin cannot continue its recovery workspace")
	}
}
