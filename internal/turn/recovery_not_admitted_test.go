package turn

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func neverAdmittedFixture(t *testing.T, ended bool) (*Coordinator, *ledger.Ledger, task.Task, Request) {
	t.Helper()
	c, tasks, book := taskCoordinatorBook(t, &fakeRunner{reply: "must not execute"}, withOwner("owner"))
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
	c, _, book := taskCoordinatorBook(t, runner, withOwner("owner"), withCallbacks(func(cb *Callbacks) { cb.AgentGate = &fakeGate{} }))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	c.runtime = failingPreparationManager{manager}
	req := Request{Channel: "console", ConversationID: "console:proof", MessageID: "web-e1", ExchangeID: "e1", SenderOpenID: "owner", Input: "normal question", Mentioned: true}
	if _, err := c.Handle(t.Context(), req); err == nil {
		t.Fatal("preparation unexpectedly succeeded")
	}
	if len(manager.opened) != 0 || len(runner.seen()) != 0 {
		t.Fatal("preparation failure admitted native execution")
	}
	var err error
	c.tasks, err = task.OpenLedger(book)
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
			c.tasks, err = task.OpenLedger(book)
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

func TestConfirmNeverAdmittedRefusesInterruptedInputEvenAfterCancel(t *testing.T) {
	c, book, tracked, req := neverAdmittedFixture(t, true)
	if _, err := book.DB().Exec(`UPDATE bindings SET data=json_set(data,'$.outcome','interrupted')
		WHERE kind='task-attempt' AND coalesce(json_extract(data,'$.execution_id'),'')=''`); err != nil {
		t.Fatal(err)
	}
	// Observe the same committed state the restarted runtime uses.
	var err error
	c.tasks, err = task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if yes, err := confirmUnadmitted(t, c, req); yes {
		t.Fatalf("interrupted running input was treated as ended: %v", err)
	}
	if _, err := c.tasks.SetAside(tracked.ID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if yes, err := confirmUnadmitted(t, c, req); yes || err != nil {
		t.Fatalf("cancelled interrupted input proof=%v err=%v", yes, err)
	}
}
