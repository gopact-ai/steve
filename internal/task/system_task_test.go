package task

import (
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
)

// A task the platform opens for itself — the onboarding introduction — is
// not work anyone asked for. It keeps its accounting and its recovery row,
// but no task list, page or count the board reads shows it, after a live
// update or after a restart rebuilds the index.
func TestSystemTaskStaysOutOfTaskListsButKeepsItsAccounting(t *testing.T) {
	s, book := taskRecordBook(t)
	user, err := s.Create(Task{Goal: "user work", Transport: "console", Channel: "console:work", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	system, err := s.Create(Task{Goal: "onboarding", Transport: "console", Channel: "steve:onboard:owner", ProjectID: "p", System: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(system.ID, "worker", "", TurnInput{Address: channel.Address{Channel: "console", Conversation: system.Channel, Message: "intro"}}); err != nil {
		t.Fatal(err)
	}
	open := false
	for _, candidate := range s.OpenPrimaryAccounting() {
		open = open || candidate.Task.ID == system.ID
	}
	if !open {
		t.Fatal("system task's open row is not offered to recovery")
	}
	if _, err := s.Finish(system.ID, OutcomeOK, Tokens{Input: 3, Output: 4, Total: 7}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(system.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	assertReadIndexMatchesStartup(t, s)

	check := func(t *testing.T, s *Store) {
		t.Helper()
		for _, scope := range []Scope{{}, {Kind: "project", ID: "p"}, {Kind: "conversation", ID: system.Channel}, {Kind: "children"}} {
			for _, status := range []string{"all", "live", "closed"} {
				page, err := s.Query(Query{Scope: scope, Status: status})
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range page.Items {
					if item.ID == system.ID {
						t.Fatalf("scope %+v status %s lists the system task: %+v", scope, status, page.Items)
					}
				}
			}
		}
		for _, scope := range []Scope{{}, {Kind: "project", ID: "p"}} {
			if counts := s.Counts(scope); counts.Total != 1 || counts.Roots != 1 || counts.CompletedRoots != 0 {
				t.Fatalf("scope %+v counts the system task: %+v", scope, counts)
			}
		}
		for _, item := range s.Workset(nil).Items {
			if item.ID == system.ID {
				t.Fatal("the board's work set shows the system task")
			}
		}
		page, err := s.Query(Query{})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != user.ID {
			t.Fatalf("user task list = %+v, %v; want only the user's task", page.Items, err)
		}
		head, ok := s.Header(system.ID)
		if !ok || !head.System || head.Summary.Tokens.Total != 7 || head.Summary.Attempts != 1 {
			t.Fatalf("system task header = %+v, %v; want it kept, marked and accounted", head, ok)
		}
	}
	check(t, s)
	reopened, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	check(t, reopened)
}
