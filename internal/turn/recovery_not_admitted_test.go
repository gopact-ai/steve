package turn

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func neverAdmittedFixture(t *testing.T, ended bool) (*Coordinator, *ledger.Ledger, task.Task, Request) {
	t.Helper()
	c, tasks, book := taskCoordinatorBook(t, &fakeRunner{reply: "must not execute"})
	c.SetIdentity("owner", nil)
	req := Request{Channel: "console", ConversationID: "console:proof", MessageID: "web-e1", ExchangeID: "e1", SenderOpenID: "owner", ExpectedProject: "codex"}
	tracked, err := tasks.Create(task.Task{Transport: req.Channel, Channel: req.ConversationID, Requester: "owner", Member: "codex", ProjectID: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedTask = tracked.ID
	tracked, err = tasks.BeginTurn(tracked.ID, "codex", "node", task.TurnInput{Address: req.Address(), TurnID: req.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	if ended {
		tracked, err = tasks.Finish(tracked.ID, task.OutcomeError, task.Tokens{}, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	return c, book, tracked, req
}

type failingPreparationManager struct{ *fakeManager }

func (m failingPreparationManager) SupportsHTTPMCP(context.Context, harness.Placement) (bool, error) {
	return false, errors.New("session capability preparation failed")
}

func TestConfirmNeverAdmittedAfterRealPrepareSessionErrorAndRestart(t *testing.T) {
	runner := &fakeRunner{reply: "must not run"}
	c, _, book := taskCoordinatorBook(t, runner)
	c.SetIdentity("owner", nil)
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	c.runtime = failingPreparationManager{manager}
	c.SetAgentGate(&fakeGate{})
	req := Request{Channel: "console", ConversationID: "console:proof", MessageID: "web-e1", ExchangeID: "e1", SenderOpenID: "owner", Input: "normal question", Mentioned: true}
	if _, err := c.Handle(t.Context(), req); err == nil {
		t.Fatal("preparation unexpectedly succeeded")
	}
	if len(manager.opened) != 0 || len(runner.seen()) != 0 {
		t.Fatal("preparation failure admitted native execution")
	}
	var err error
	c.tasks, err = task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	c.attempts = attempt.New(book)
	if confirmed, err := confirmUnadmitted(t, c, req); err != nil || !confirmed {
		t.Fatalf("restarted preparation failure cannot finish: confirmed=%v err=%v", confirmed, err)
	}
}

func confirmUnadmitted(t *testing.T, c *Coordinator, req Request) (bool, error) {
	t.Helper()
	proof, ok := any(c).(interface {
		ConfirmNeverAdmitted(context.Context, Request) (bool, error)
	})
	if !ok {
		t.Fatal("coordinator does not provide durable never-admitted proof")
	}
	return proof.ConfirmNeverAdmitted(t.Context(), req)
}

func TestConfirmNeverAdmittedRequiresPositiveEndedAccounting(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "unfinished", true: "ended"}[ended], func(t *testing.T) {
			c, book, _, req := neverAdmittedFixture(t, ended)
			// Re-open the owners: neither an old runner nor in-memory task state
			// is the source of the proof.
			var err error
			c.tasks, err = task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			c.attempts = attempt.New(book)
			got, err := confirmUnadmitted(t, c, req)
			if err != nil || got != ended {
				t.Fatalf("confirmed=%v ended=%v err=%v", got, ended, err)
			}
		})
	}
}

func TestConfirmNeverAdmittedRejectsUnrelatedIdentityAndActiveObserver(t *testing.T) {
	for _, field := range []string{"channel", "conversation", "message", "exchange", "requester", "project", "task", "origin", "active"} {
		t.Run(field, func(t *testing.T) {
			c, _, _, req := neverAdmittedFixture(t, true)
			switch field {
			case "channel":
				req.Channel = "feishu"
			case "conversation":
				req.ConversationID = "console:other"
			case "message":
				req.MessageID = "web-other"
			case "exchange":
				req.ExchangeID = "other"
			case "requester":
				req.SenderOpenID = "other"
			case "project":
				req.ExpectedProject = "other"
			case "task":
				req.ExpectedTask = "other"
			case "origin":
				req.Origin = "another-schedule"
			case "active":
				c.cancels[sessionKey(req.ConversationID, "codex")] = &turnEntry{}
			}
			if confirmed, err := confirmUnadmitted(t, c, req); confirmed {
				t.Fatalf("wrong identity/active observer authorized closure: %v", err)
			}
		})
	}
}

func TestConfirmNeverAdmittedDoesNotUseLegacyAnchorOrMissingAccounting(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "legacy-anchor"}[legacy], func(t *testing.T) {
			c, book, _, req := neverAdmittedFixture(t, true)
			query := `DELETE FROM bindings WHERE kind='task-attempt'`
			if legacy {
				query = `UPDATE bindings SET data=json_set(data,'$.turn_id','') WHERE kind='task-attempt'`
			}
			if _, err := book.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			if got, err := confirmUnadmitted(t, c, req); got {
				t.Fatalf("weak/missing accounting became proof: %v", err)
			}
		})
	}
}

func TestConfirmNeverAdmittedRejectsAnyAdmittedAttemptEvenOutsideRetainedView(t *testing.T) {
	for _, phase := range []string{"leased", "prepared", "running", "bound", "failed", "expired", "superseded"} {
		t.Run(phase, func(t *testing.T) {
			c, book, tracked, req := neverAdmittedFixture(t, true)
			if _, err := book.DB().Exec(`INSERT INTO operations VALUES('hidden','attempt',?,1,1,?,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`,
				phase, `{"id":"hidden","task_id":"`+tracked.ID+`","turn_id":"`+req.MessageID+`","kind":"chat"}`); err != nil {
				t.Fatal(err)
			}
			if got, err := confirmUnadmitted(t, c, req); got {
				t.Fatalf("admitted %s attempt became never-admitted: %v", phase, err)
			}
		})
	}
}

func TestConfirmNeverAdmittedRejectsCorruptOrAmbiguousEvidence(t *testing.T) {
	for _, fault := range []string{"attempt-json", "attempt-envelope", "attempt-no-turn", "accounting-json", "accounting-key", "duplicate", "interrupted"} {
		t.Run(fault, func(t *testing.T) {
			c, book, tracked, req := neverAdmittedFixture(t, true)
			var err error
			switch fault {
			case "attempt-no-turn":
				_, err = book.DB().Exec(`INSERT INTO operations VALUES('unknown','attempt','bound',1,1,?,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`,
					`{"id":"unknown","task_id":"`+tracked.ID+`"}`)
			case "attempt-json", "attempt-envelope":
				data, revision := `null`, "1"
				if fault == "attempt-envelope" {
					data, revision = `{"id":"bad","task_id":"other","turn_id":"other"}`, "'invalid'"
				}
				_, err = book.DB().Exec(`INSERT INTO operations VALUES('bad','attempt','bound',`+revision+`,1,?,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`, data)
			case "accounting-json":
				_, err = book.DB().Exec(`UPDATE bindings SET data='null' WHERE kind='task-attempt'`)
			case "accounting-key":
				_, err = book.DB().Exec(`UPDATE bindings SET id='wrong' WHERE kind='task-attempt'`)
			case "duplicate":
				other, createErr := c.tasks.Create(task.Task{Transport: req.Channel, Channel: req.ConversationID, Requester: "owner", Member: "other", ProjectID: tracked.ProjectID})
				if createErr != nil {
					t.Fatal(createErr)
				}
				_, err = c.tasks.BeginTurn(other.ID, "other", "", task.TurnInput{Address: channel.Address{Channel: req.Channel, Conversation: req.ConversationID, Message: req.MessageID}})
			case "interrupted":
				_, err = book.DB().Exec(`UPDATE bindings SET data=json_set(data,'$.outcome','interrupted') WHERE kind='task-attempt'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := confirmUnadmitted(t, c, req); got {
				t.Fatalf("invalid evidence became proof: %v", err)
			}
		})
	}
}

func cancelledLegacyFixture(t *testing.T, cancel bool) (*Coordinator, *ledger.Ledger, task.Task, Request) {
	t.Helper()
	c, tasks, book := taskCoordinatorBook(t, &fakeRunner{reply: "must not execute"})
	c.SetIdentity("owner", nil)
	req := Request{Channel: "console", ConversationID: "console:proof", MessageID: "web-e1", ExchangeID: "e1", SenderOpenID: "owner", ExpectedProject: "codex"}
	tracked, err := tasks.Create(task.Task{Transport: req.Channel, Channel: req.ConversationID, Requester: "owner", Member: "codex", ProjectID: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedTask = tracked.ID
	for i := range 3 {
		turnID := fmt.Sprintf("web-history-%d", i)
		address := req.Address()
		address.Message = turnID
		if _, err := tasks.BeginTurn(tracked.ID, "codex", "", task.TurnInput{Address: address}); err != nil {
			t.Fatal(err)
		}
		token, err := tasks.ExecutionToken(tracked.ID)
		if err != nil {
			t.Fatal(err)
		}
		record, err := c.attempts.Open(t.Context(), attempt.Spec{ID: fmt.Sprintf("history-%d", i), TaskID: tracked.ID, TurnID: turnID,
			Kind: attempt.KindChat, Execution: &token, Scope: attempt.ScopeNone, Project: "codex", Agent: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
			t.Fatal(err)
		}
		for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
			if _, err := c.attempts.Advance(t.Context(), record.ID, phase, "test", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.attempts.MarkSessionSettled(t.Context(), record.ID, "test"); err != nil {
			t.Fatal(err)
		}
		for _, phase := range []attempt.State{attempt.BindReady, attempt.Bound} {
			if _, err := c.attempts.Advance(t.Context(), record.ID, phase, "test", nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tasks.Finish(tracked.ID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Reproduce the old accounting format through owner APIs: the input is
	// anchored but its row has not acquired a TurnID from BindAttempt.
	if _, err := tasks.Begin(tracked.ID, "codex", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SetAnchor(tracked.ID, req.Address(), "console", "p2p", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Finish(tracked.ID, task.OutcomeError, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if cancel {
		control := req
		control.Input, control.MessageID, control.ExchangeID = "/tasks cancel "+tracked.ID, "web-control", "control"
		if _, err := c.Handle(t.Context(), control); err != nil {
			t.Fatal(err)
		}
	}
	tracked, _ = tasks.Get(tracked.ID)
	return c, book, tracked, req
}

func TestConfirmNeverAdmittedLegacyRequiresExplicitCancellationAndSettledNamedHistory(t *testing.T) {
	c, book, tracked, req := cancelledLegacyFixture(t, false)
	saved := state.Session{ConversationID: req.ConversationID, AgentID: "codex", HarnessID: "codex", UpstreamID: "original-native-context", AgentToken: "old-session-token"}
	if err := c.store.SaveSession(saved); err != nil {
		t.Fatal(err)
	}
	if yes, err := confirmUnadmitted(t, c, req); yes {
		t.Fatalf("running legacy anchor became proof: %v", err)
	}
	control := req
	control.Input, control.MessageID, control.ExchangeID = "/tasks cancel "+tracked.ID, "web-control", "control"
	if _, err := c.Handle(t.Context(), control); err != nil {
		t.Fatal(err)
	}
	stopped, _ := c.tasks.Get(tracked.ID)
	if stopped.State != task.StateCancelled || stopped.ExecutionEpoch <= tracked.ExecutionEpoch {
		t.Fatalf("cancel did not fence original task: %+v", stopped)
	}
	if current := c.store.Conversation(req.ConversationID).Sessions["codex"]; current.UpstreamID != saved.UpstreamID || current.AgentToken != saved.AgentToken {
		t.Fatalf("task cancellation replaced or discarded native context: %+v", current)
	}
	if yes, err := confirmUnadmitted(t, c, req); !yes || err != nil {
		t.Fatalf("explicitly cancelled unadmitted legacy input cannot close: %v %v", yes, err)
	}
	var err error
	c.tasks, err = task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	c.attempts = attempt.New(book)
	if yes, err := confirmUnadmitted(t, c, req); !yes || err != nil {
		t.Fatalf("cancelled legacy proof lost after restart: %v %v", yes, err)
	}
	if _, err := c.tasks.Resume(stopped.ID, stopped.ExecutionEpoch, stopped.State, task.ResumeAdmission{}); err == nil {
		t.Fatal("cancelled proof allowed resume of original task")
	}
}

func TestConfirmNeverAdmittedLegacyRejectsUnknownHistoryAndWeakFences(t *testing.T) {
	for _, fault := range []string{"unnamed", "unsettled", "running-attempt", "missing-settlement", "same-input", "duplicate-history-turn",
		"missing-accounting", "missing-attempt", "historical-epoch", "historical-task", "ambiguous-anchor", "child", "paused", "same-epoch", "unfinished-row", "session", "wrong-owner", "active"} {
		t.Run(fault, func(t *testing.T) {
			c, book, tracked, req := cancelledLegacyFixture(t, true)
			query := ""
			switch fault {
			case "unnamed":
				query = `UPDATE operations SET data=json_set(data,'$.turn_id','') WHERE id='history-0'`
			case "unsettled":
				query = `UPDATE operations SET data=json_set(data,'$.unsettled',json('true')) WHERE id='history-0'`
			case "running-attempt":
				query = `UPDATE operations SET state='running' WHERE id='history-0'`
			case "missing-settlement":
				query = `UPDATE operations SET data=json_remove(data,'$.session_settled') WHERE id='history-0'`
			case "same-input":
				query = `UPDATE operations SET data=json_set(data,'$.turn_id','web-e1') WHERE id='history-0'`
			case "duplicate-history-turn":
				query = `UPDATE operations SET data=json_set(data,'$.turn_id','web-history-1') WHERE id='history-0'`
			case "missing-accounting":
				query = `DELETE FROM bindings WHERE kind='task-attempt' AND json_extract(data,'$.execution_id')='history-0'`
			case "missing-attempt":
				query = `DELETE FROM operations WHERE id='history-0'`
			case "historical-epoch":
				query = `UPDATE operations SET data=json_set(data,'$.execution.epoch',999) WHERE id='history-0'`
			case "historical-task":
				query = `UPDATE operations SET data=json_set(data,'$.execution.task_id','other') WHERE id='history-0'`
			case "ambiguous-anchor", "child":
				other := task.Task{Transport: req.Channel, Channel: req.ConversationID, AnchorMessage: req.MessageID, Requester: "owner", Member: "other", ProjectID: tracked.ProjectID}
				if fault == "child" {
					other.Parent, other.AnchorMessage = tracked.ID, "another-anchor"
				}
				if _, err := c.tasks.Create(other); err != nil {
					t.Fatal(err)
				}
			case "paused":
				query = `UPDATE bindings SET data=json_set(data,'$.state','paused') WHERE kind='task'`
			case "same-epoch":
				query = `UPDATE bindings SET data=json_set(data,'$.execution_epoch',1) WHERE kind='task'`
			case "unfinished-row":
				query = `UPDATE bindings SET data=json_remove(data,'$.ended_at') WHERE kind='task-attempt' AND json_extract(data,'$.index')=3`
			case "session":
				query = `UPDATE bindings SET data=json_set(data,'$.session','unknown') WHERE kind='task-attempt' AND json_extract(data,'$.index')=3`
			case "wrong-owner":
				req.SenderOpenID = "other"
			case "active":
				c.cancels[sessionKey(req.ConversationID, tracked.Member)] = &turnEntry{}
			}
			if query != "" {
				if _, err := book.DB().Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			if yes, err := confirmUnadmitted(t, c, req); yes {
				t.Fatalf("%s was treated as cancelled never-admitted proof: %v", fault, err)
			}
		})
	}
}

func TestConfirmNeverAdmittedInterruptedRequiresCancelledLegacyReconciliation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "named", true: "legacy"}[legacy], func(t *testing.T) {
			var c *Coordinator
			var book *ledger.Ledger
			var tracked task.Task
			var req Request
			if legacy {
				c, book, tracked, req = cancelledLegacyFixture(t, false)
			} else {
				c, book, tracked, req = neverAdmittedFixture(t, true)
			}
			if _, err := book.DB().Exec(`UPDATE bindings SET data=json_set(data,'$.outcome','interrupted')
				WHERE kind='task-attempt' AND coalesce(json_extract(data,'$.execution_id'),'')=''`); err != nil {
				t.Fatal(err)
			}
			// Observe the same committed state the restarted runtime uses.
			var err error
			c.tasks, err = task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			if yes, err := confirmUnadmitted(t, c, req); yes {
				t.Fatalf("interrupted running input was treated as ended: %v", err)
			}
			if _, err := c.tasks.SetAside(tracked.ID, task.StateCancelled); err != nil {
				t.Fatal(err)
			}
			yes, err := confirmUnadmitted(t, c, req)
			if err != nil || yes != legacy {
				t.Fatalf("cancelled interrupted input proof=%v legacy=%v err=%v", yes, legacy, err)
			}
			if legacy {
				if _, err := book.DB().Exec(`UPDATE operations SET data=json_set(data,'$.unsettled',json('true')) WHERE id='history-0'`); err != nil {
					t.Fatal(err)
				}
				if yes, err := confirmUnadmitted(t, c, req); yes {
					t.Fatalf("interrupted legacy proof skipped historical settlement: %v", err)
				}
			}
		})
	}
}
