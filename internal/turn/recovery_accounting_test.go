package turn

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/task"
)

func TestRejectedOnTurnReadyReceiptCancelsBeforeNativePrompt(t *testing.T) {
	runner := &fakeRunner{reply: "must not execute"}
	c, _, book := taskCoordinatorBook(t, runner)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_channel_attempt BEFORE INSERT ON commands
		WHEN NEW.kind='gateway-input-attempt' BEGIN SELECT RAISE(ABORT,'admission receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var receiptErr error
	var admitted string
	result, err := c.Handle(ctx, Request{ConversationID: "chat", MessageID: "original-message", Input: "must not execute", OnTurnReady: func(taskID, attemptID string) {
		admitted = attemptID
		raw, marshalErr := json.Marshal(map[string]string{"task_id": taskID, "attempt_id": attemptID})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		receiptErr = book.RecordCommand(ctx, "input/attempt", "gateway-input-attempt", "owner", raw)
		if receiptErr != nil {
			cancel()
		}
	}})
	if admitted == "" || receiptErr == nil || !strings.Contains(receiptErr.Error(), "admission receipt unavailable") {
		t.Fatalf("fault missed durable callback boundary: admitted=%s error=%v", admitted, receiptErr)
	}
	if err == nil || len(runner.seen()) != 0 || result.Text == "must not execute" {
		t.Fatalf("native input escaped failed receipt: prompts=%v result=%+v err=%v", runner.seen(), result, err)
	}
}

func TestCompletionCallbackWaitsForDurableTaskAccounting(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "accounting-failure"}[fail], func(t *testing.T) {
			runner := &fakeRunner{reply: "original completed answer"}
			afterTurn := func(string) {}
			c, book := completionCoordinator(t, runner, withCallbacks(func(cb *Callbacks) {
				cb.AfterTurn = func(id string) { afterTurn(id) }
			}))
			called := 0
			afterTurn = func(id string) {
				called++
				durable, err := task.OpenLedger(book)
				if err != nil {
					t.Fatal(err)
				}
				tracked, _ := durable.Get(id)
				if tracked.HasOpenExecution() {
					t.Error("continuation callback ran before accounting commit")
				}
			}
			if fail {
				_, err := book.DB().Exec(`CREATE TRIGGER reject_completion_accounting BEFORE INSERT ON bindings WHEN NEW.kind='task-attempt' AND json_extract(NEW.data,'$.ended_at') IS NOT NULL BEGIN SELECT RAISE(ABORT,'completion accounting unavailable'); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			result, err := handle(c, t.Context(), "complete this work")
			if fail {
				if err == nil || !strings.Contains(err.Error(), "completion accounting unavailable") || result.Attempt != "" {
					t.Errorf("failed accounting was delivered: result=%+v err=%v", result, err)
				}
				if called != 0 {
					t.Errorf("failed accounting triggered %d callbacks", called)
				}
				if len(runner.seen()) != 1 {
					t.Fatal("test did not execute once")
				}
			} else if err != nil || called != 1 {
				t.Fatalf("completion: calls=%d err=%v", called, err)
			}
		})
	}
}

func TestRetainedCallbackWaitsForDurableAccounting(t *testing.T) {
	afterTurn := func(string) {}
	c, _, _, old, req := retainedChatFixture(t, withCallbacks(func(cb *Callbacks) {
		cb.AfterTurn = func(id string) { afterTurn(id) }
	}))
	called := 0
	afterTurn = func(id string) {
		called++
		tracked, _ := c.tasks.Get(id)
		if tracked.HasOpenExecution() {
			t.Error("retained callback preceded accounting")
		}
	}
	if _, err := c.ResumeRetainedChat(t.Context(), old.ID, req); err != nil {
		t.Fatal(err)
	}
	record, err := c.attempts.Get(t.Context(), old.ID)
	if err != nil || record.State != attempt.Bound || called != 1 {
		t.Fatalf("bound=%s calls=%d err=%v", record.State, called, err)
	}
}

func TestRetainedAccountingRetryResolvesOnlyJoinedOriginalEpoch(t *testing.T) {
	c, _, book, old, req := retainedChatFixture(t)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_retained_accounting BEFORE INSERT ON bindings
		WHEN NEW.kind='task-attempt' AND json_extract(NEW.data,'$.ended_at') IS NOT NULL
		BEGIN SELECT RAISE(ABORT,'retained accounting unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResumeRetainedChat(t.Context(), old.ID, req); err == nil {
		t.Fatal("accounting failure hidden")
	}
	if got := c.executions.Active(); len(got) == 0 {
		t.Fatal("fixture did not retain the unresolved observer")
	}
	if err := c.SettleChatAccounting(t.Context(), old.ID); err == nil {
		t.Fatal("uncommitted accounting resolved")
	}
	if got := c.executions.Active(); len(got) == 0 {
		t.Fatal("failed settlement cleared unresolved")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_retained_accounting`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.Advance(old.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	newScope, err := c.executions.Begin(t.Context(), execution.Key{TaskID: old.TaskID, AttemptID: old.ID, InstanceID: "new-epoch"})
	if err != nil {
		t.Fatal(err)
	}
	newScope.Finish(errors.New("new epoch remains unresolved"))
	if err := c.SettleChatAccounting(t.Context(), old.ID); err != nil {
		t.Fatal(err)
	}
	if got := c.executions.Active(); !reflect.DeepEqual(got, []string{"new-epoch"}) {
		t.Fatalf("old completion did not resolve exactly its joined observer: %v", got)
	}
}

func TestCompletedRelocationRequiresAccountingBeforeResult(t *testing.T) {
	c, _, book, old, req := retainedChatFixture(t)
	if _, err := c.ResumeRetainedChat(t.Context(), old.ID, req); err != nil {
		t.Fatal(err)
	}
	record, err := c.attempts.Get(t.Context(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Reopen only the accounting projection to reproduce the crash window,
	// leaving the actual execution's committed output and settlement intact.
	if _, err := book.DB().Exec(`UPDATE bindings SET data=json_remove(data,'$.ended_at','$.usage_known')
		WHERE kind='task-attempt' AND json_extract(data,'$.execution_id')=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	c.tasks, err = task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_relocation_accounting BEFORE INSERT ON bindings
		WHEN NEW.kind='task-attempt' AND json_extract(NEW.data,'$.ended_at') IS NOT NULL
		BEGIN SELECT RAISE(ABORT,'relocation accounting unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if result, err := c.completedRelocation(t.Context(), req, record); err == nil || result.Attempt != "" {
		t.Fatalf("relocation result escaped pending accounting: %+v %v", result, err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_relocation_accounting`); err != nil {
		t.Fatal(err)
	}
	if result, err := c.completedRelocation(t.Context(), req, record); err != nil || result.Attempt != old.ID {
		t.Fatalf("relocation did not deliver original result: %+v %v", result, err)
	}
}
