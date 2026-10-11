package app

import (
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"net/http"
	"path/filepath"
	"testing"
)

func TestNativeMCPRechecksAccountRevisionAndProjectGrant(t *testing.T) {
	for _, revoke := range []string{"account", "project"} {
		t.Run(revoke, func(t *testing.T) {
			book, tasks := applicationGrantBook(t)
			accounts := &identity.Accounts{Book: book}
			if err := accounts.Bootstrap(t.Context(), identity.Configuration{ConsoleOwner: "owner"}); err != nil {
				t.Fatal(err)
			}
			projects := project.Open(book)
			if err := projects.SetIdentityPolicy(t.Context(), accounts, "third"); err != nil {
				t.Fatal(err)
			}
			if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}, DefaultRole: project.RoleNone}}); err != nil {
				t.Fatal(err)
			}
			account := identity.Account{Channel: "third", ID: "actor"}
			if _, err := projects.Grant(t.Context(), "p", account.Principal(), project.RoleWrite, "owner"); err != nil {
				t.Fatal(err)
			}
			tracked, err := tasks.Create(task.Task{Channel: "chat", Transport: "third", Requester: "actor", Member: "agent", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Begin(tracked.ID, "agent", "node-a", ""); err != nil {
				t.Fatal(err)
			}
			token, err := tasks.ExecutionToken(tracked.ID)
			if err != nil {
				t.Fatal(err)
			}
			attempts := attempt.New(book)
			record, err := attempts.Open(t.Context(), attempt.Spec{ID: "original", TaskID: tracked.ID, TurnID: "native-input", By: "actor", Execution: &token, Kind: attempt.KindChat, Project: "p", Node: "node-a", Agent: "agent", Harness: "test", Scope: attempt.ScopeNone, Workspace: project.Workspace{ID: "work", Project: "p", Node: "node-a", Kind: project.KindWorktree, Path: filepath.Join(t.TempDir(), "work")}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := attempts.Advance(t.Context(), record.ID, attempt.Prepared, "test", nil); err != nil {
				t.Fatal(err)
			}
			record, err = attempts.RecordSession(t.Context(), record.ID, "test", "ns_original")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := attempts.Advance(t.Context(), record.ID, attempt.Running, "test", nil); err != nil {
				t.Fatal(err)
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return identity.BindExecutionTx(tx, record.ID, account) }); err != nil {
				t.Fatal(err)
			}
			gate := applicationGrantGate(t, book)
			gate.Extras("chat", "agent", "native-token", "")
			scope := agentmcp.GrantScope{TaskID: tracked.ID, TaskEpoch: token.Epoch, AttemptID: record.ID, ExecutionGeneration: attempt.SessionExecutionEpoch(record), NodeID: record.Node, SessionID: record.Session}
			if err := gate.BindExecution(t.Context(), agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
				t.Fatal(err)
			}
			if status := grantRPC(t, gate, "native-token", "tools/call"); status != http.StatusOK {
				t.Fatalf("original authority refused: %d", status)
			}
			if revoke == "account" {
				if _, err := accounts.Bind(t.Context(), account, "", 0, false); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := projects.Grant(t.Context(), "p", account.Principal(), project.RoleNone, "owner"); err != nil {
					t.Fatal(err)
				}
			}
			if status := grantRPC(t, gate, "native-token", "tools/call"); status != http.StatusUnauthorized {
				t.Fatalf("old MCP token retained revoked %s authority: %d", revoke, status)
			}
			still, err := attempts.Get(t.Context(), record.ID)
			if err != nil || still.By != "actor" || still.ID != record.ID || still.State != attempt.Running {
				t.Fatal("revocation rewrote or replaced original execution")
			}
		})
	}
}
