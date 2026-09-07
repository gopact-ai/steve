package main

import (
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/project"
)

func TestCommittedSessionCleanupUsesNativeIdentityWithoutWorkdir(t *testing.T) {
	record := attempt.Record{Spec: attempt.Spec{Node: "node-a", Harness: "test", Workspace: project.Workspace{Path: "/fixture/worktree"}}, Session: "ns_original"}
	place := harness.Placement{Node: "node-a", Harness: "test"}
	if err := validateSessionPlacement(record, place, "ns_original", ""); err != nil {
		t.Fatalf("committed native cleanup refused: %v", err)
	}
	for _, id := range []string{"ns_other", "raw-session"} {
		if err := validateSessionPlacement(record, place, id, ""); err == nil {
			t.Fatalf("empty workdir accepted for unrelated native identity %q", id)
		}
	}
	if err := validateSessionPlacement(record, place, "", ""); err != nil {
		t.Fatalf("admitted capability inquiry refused: %v", err)
	}
	if err := validateSessionPlacement(record, place, "ns_original", "/another/worktree"); err == nil {
		t.Fatal("mismatched workspace accepted")
	}
	if err := validateSessionPlacement(record, harness.Placement{Node: "node-b", Harness: "test"}, "ns_original", ""); err == nil {
		t.Fatal("cleanup accepted on wrong physical machine")
	}
	if err := validateSessionPlacement(record, place, "", "/fixture/worktree"); err != nil {
		t.Fatalf("admitted new session refused: %v", err)
	}
}
