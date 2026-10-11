package control

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/conversation"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turnapi"
	"github.com/gopact-ai/steve/internal/work"
)

func controlFixture(t *testing.T) (*Service, *identity.Accounts, turnapi.Request) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	accounts := &identity.Accounts{Book: book}
	config := identity.Configuration{ConsoleOwner: "owner", Accounts: accounts}
	if err := accounts.Bootstrap(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	owners, err := identity.NewOwners(config)
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	if err := projects.SetIdentityPolicy(t.Context(), accounts, "im"); err != nil {
		t.Fatal(err)
	}
	if err := projects.Declare(t.Context(), []project.Project{
		{ID: "p", Home: project.Home{Path: t.TempDir()}, DefaultRole: project.RoleNone},
		{ID: "private", Home: project.Home{Path: t.TempDir()}, DefaultRole: project.RoleNone},
	}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	schedules, err := schedule.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Owners: owners, Projects: projects, Tasks: tasks, Schedules: schedules, DefaultProject: "p"}
	req := turnapi.Request{Source: turnapi.Source{Channel: "im", ConversationID: "console:opaque", MessageID: "control", ChatType: protocol.ChatP2P}, Actor: turnapi.Actor{ID: "actor"}}
	if _, err := projects.Grant(t.Context(), "p", identity.Account{Channel: "im", ID: "actor"}.Principal(), project.RoleWrite, accounts.Owner); err != nil {
		t.Fatal(err)
	}
	if _, err := projects.Bind(t.Context(), req.Source.ConversationID, "p", "fixture"); err != nil {
		t.Fatal(err)
	}
	acceptControl(t, s, req)
	return s, accounts, req
}

func acceptControl(t *testing.T, s *Service, req turnapi.Request) {
	t.Helper()
	if err := s.book().Update(t.Context(), func(tx *ledger.Tx) error {
		if err := conversation.BindRouteTx(tx, req.Source.ConversationID, req.Source.Channel); err != nil {
			return err
		}
		a := identity.Account{Channel: req.Source.Channel, ID: req.Actor.ID}
		binding, err := identity.ResolveAccount(tx, a)
		if err != nil {
			return err
		}
		id := durable.Key(req.Source.Channel, req.Source.ConversationID, req.Source.MessageID)
		return tx.PutBinding(work.InputKind, id, work.InputRecord{ID: id, Address: req.Address(), Actor: a, ChatType: string(req.Source.ChatType), Principal: binding.Principal, AccountRevision: binding.Revision, State: work.InputRunning})
	}); err != nil {
		t.Fatal(err)
	}
}

// The permission change happens after admission and after the guard is built.
// The actual task/session commit must refuse it without altering either cache.
func TestControlsRecheckRevocationAtCommit(t *testing.T) {
	for _, revoke := range []string{"account", "project"} {
		t.Run(revoke, func(t *testing.T) {
			s, accounts, req := controlFixture(t)
			tracked, err := s.CreateTask(t.Context(), req, task.Task{Channel: req.Source.ConversationID, Transport: "im", Member: "agent", Requester: req.Actor.ID, ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Tasks.Advance(tracked.ID, task.StateRunning); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Tasks.Get(tracked.ID)
			guard := s.TaskGuard(req, tracked.ID, project.RoleWrite)
			selectionGuard := s.Guard(req, project.RoleWrite)
			a := identity.Account{Channel: "im", ID: "actor"}
			if revoke == "account" {
				if _, err := accounts.Bind(t.Context(), a, "", 0, false); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.Projects.Grant(t.Context(), "p", a.Principal(), project.RoleRead, accounts.Owner); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Tasks.SetAsideChecked(t.Context(), tracked.ID, task.StatePaused, guard); !errors.Is(err, ErrDenied) {
				t.Fatalf("revoked task control: %v", err)
			}
			after, _ := s.Tasks.Get(tracked.ID)
			if after.State != before.State || after.ExecutionEpoch != before.ExecutionEpoch {
				t.Fatal("refused control revoked or moved the original execution")
			}
			sessions, err := state.OpenLedger(accounts.Book)
			if err != nil {
				t.Fatal(err)
			}
			if err := sessions.SetActiveAgentChecked(t.Context(), req.Source.ConversationID, "agent", selectionGuard); !errors.Is(err, ErrDenied) {
				t.Fatalf("revoked selection: %v", err)
			}
			if sessions.Conversation(req.Source.ConversationID).ActiveAgent != "" {
				t.Fatal("refused selection changed the live cache")
			}
		})
	}
}

func TestControlsUseCallerResourceAndAdmittedTransport(t *testing.T) {
	s, accounts, req := controlFixture(t)
	if err := s.Owner(t.Context(), req, true); !errors.Is(err, ErrDenied) {
		t.Fatalf("native console prefix acquired owner: %v", err)
	}
	visible, err := s.ProjectsFor(t.Context(), req)
	if err != nil || len(visible) != 1 || visible[0].ID != "p" {
		t.Fatalf("projects exposed an unreadable resource: %v %v", visible, err)
	}
	background, err := s.Tasks.Create(task.Task{Transport: "im", Member: "agent", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(t.Context(), req, background.ID, project.RoleWrite); !errors.Is(err, ErrDenied) {
		t.Fatalf("conversation took over an unaddressed task: %v", err)
	}
	job, err := s.CreateSchedule(t.Context(), req, schedule.Job{Channel: "im", ConversationID: req.Source.ConversationID, Requester: "actor", ProjectID: "p", Spec: schedule.Spec{Kind: schedule.KindEvery, Every: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	other := req
	other.Actor.ID, other.Source.MessageID = "other", "other-control"
	if _, err := s.Projects.Grant(t.Context(), "p", identity.Account{Channel: "im", ID: "other"}.Principal(), project.RoleWrite, accounts.Owner); err != nil {
		t.Fatal(err)
	}
	acceptControl(t, s, other)
	if _, _, err := s.CancelSchedule(t.Context(), other, job.ID); !errors.Is(err, ErrDenied) {
		t.Fatalf("another project writer cancelled standing work: %v", err)
	}
	if _, found := s.Schedules.Get(job.ID); !found {
		t.Fatal("refused cancellation removed the schedule")
	}
	if _, found, err := s.CancelSchedule(t.Context(), req, job.ID); err != nil || !found {
		t.Fatalf("original caller could not cancel: found=%v error=%v", found, err)
	}
	forged := req
	forged.Source.Channel = "console"
	if err := s.book().Update(t.Context(), func(tx *ledger.Tx) error { _, _, err := s.CallerTx(tx, forged); return err }); !errors.Is(err, ErrDenied) {
		t.Fatalf("same opaque conversation on another transport authorized: %v", err)
	}
}

func TestConversationMutationGuardPinsTheProjectBinding(t *testing.T) {
	s, _, req := controlFixture(t)
	guard := s.Guard(req, project.RoleWrite)
	if _, err := s.Projects.Bind(t.Context(), req.Source.ConversationID, "private", "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := s.book().Update(t.Context(), guard); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("a stale session write followed a new project binding: %v", err)
	}
}

func TestExecutionOwnerUsesOriginalPrivateInput(t *testing.T) {
	for _, original := range []protocol.ChatType{protocol.ChatGroup, protocol.ChatP2P, ""} {
		t.Run(string(original), func(t *testing.T) {
			s, accounts, req := controlFixture(t)
			if _, err := accounts.Bind(t.Context(), identity.Account{Channel: "im", ID: "actor"}, accounts.Owner, 0, true); err != nil {
				t.Fatal(err)
			}
			req.Source.ChatType = original
			acceptControl(t, s, req)
			tracked, err := s.CreateTask(t.Context(), req, task.Task{Channel: req.Source.ConversationID, Transport: "im", ChatType: string(original), Member: "agent", Requester: "actor", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Tasks.Advance(tracked.ID, task.StateRunning); err != nil {
				t.Fatal(err)
			}
			token, err := s.Tasks.ExecutionToken(tracked.ID)
			if err != nil {
				t.Fatal(err)
			}
			attempts := attempt.New(accounts.Book)
			r, err := attempts.Open(t.Context(), attempt.Spec{ID: "original", TaskID: tracked.ID, TurnID: req.Source.MessageID, By: "actor", Execution: &token, Kind: attempt.KindChat, Project: "p", Agent: "agent", Harness: "test", Scope: attempt.ScopeNone, Workspace: project.Workspace{ID: "work", Project: "p", Kind: project.KindWorktree, Path: t.TempDir()}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := attempts.Advance(t.Context(), r.ID, attempt.Prepared, "test", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := attempts.RecordSession(t.Context(), r.ID, "test", "ns_original"); err != nil {
				t.Fatal(err)
			}
			if _, err := attempts.Advance(t.Context(), r.ID, attempt.Running, "test", nil); err != nil {
				t.Fatal(err)
			}
			if err := accounts.Book.Update(t.Context(), func(tx *ledger.Tx) error {
				return identity.BindExecutionTx(tx, r.ID, identity.Account{Channel: "im", ID: "actor"})
			}); err != nil {
				t.Fatal(err)
			}
			laterType := protocol.ChatP2P
			if original == protocol.ChatP2P {
				laterType = protocol.ChatGroup
			}
			if err := s.Tasks.SetAnchor(tracked.ID, req.Address(), "chat", string(laterType), ""); err != nil {
				t.Fatal(err)
			}
			err = accounts.Book.Read(t.Context(), func(tx *ledger.ReadTx) error { return s.ExecutionOwnerTx(tx, r.ID, false) })
			if original == protocol.ChatP2P && err != nil {
				t.Fatalf("original private owner lost authority to a later group input: %v", err)
			}
			if original != protocol.ChatP2P && !errors.Is(err, ErrDenied) {
				t.Fatalf("later private input upgraded original %q: %v", original, err)
			}
		})
	}
}
