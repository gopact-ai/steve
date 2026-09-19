package node

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestSessionCommitFreezesReceiptBeforeProgressRebindOrCleanup(t *testing.T) {
	one := progressSession(t)
	next := one.copyLocked()
	next.State.ContextID = "native-context"
	command := next.Commands[next.CurrentCommand]
	command.State, command.Settled, command.Output = nodewire.SessionCommandCompleted, true, "answer"
	next.Commands[next.CurrentCommand] = command
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	receipt := one.record.Commands[one.record.CurrentCommand].Receipt
	if err := receipt.Validate(); err != nil {
		t.Fatalf("terminal transition did not store receipt: %v", err)
	}
	next = one.copyLocked()
	next.State.Progress.Answer = "later presentation"
	next.State.ProcessStopped = true
	command = next.Commands[next.CurrentCommand]
	command.ProcessStopped, command.CancelRequested = true, true
	next.Commands[next.CurrentCommand] = command
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	if got := one.record.Commands[one.record.CurrentCommand].Receipt; got != receipt {
		t.Fatalf("cleanup changed the immutable receipt: %+v != %+v", got, receipt)
	}
	if got := savedProgress(t, one).Commands[one.record.CurrentCommand].Receipt; got != receipt {
		t.Fatalf("receipt was not atomic with durable terminal command: %+v", got)
	}
	next = one.copyLocked()
	command = next.Commands[next.CurrentCommand]
	command.Output = "rewritten result"
	next.Commands[next.CurrentCommand] = command
	if err := one.commitLocked(next); err == nil {
		t.Fatal("sealed terminal output could be rewritten")
	}
}

func TestLateNativeQuestionCannotAlterAnotherBinding(t *testing.T) {
	one, request, _ := ackFixture(t)
	next := one.copyLocked()
	next.State.Binding.AttemptID = "later"
	next.BindingInputStart = next.State.InputAccepted
	next.State.InputAccepted++
	next.CurrentCommand = "later-input"
	next.State.Questions = nil
	next.Commands[next.CurrentCommand] = nodewire.SessionCommand{ID: next.CurrentCommand, InputSequence: 2, State: nodewire.SessionCommandRunning}
	next.CommandHashes[next.CurrentCommand] = "later-hash"
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	one.waiters = make(map[string]chan struct{})
	before, changed := one.copyLocked(), one.changed
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := one.waitQuestion(ctx, nodewire.SessionQuestion{CommandID: request.Receipt.CommandID}); err == nil {
		t.Fatal("old native callback joined the new input")
	}
	if !reflect.DeepEqual(before, one.copyLocked()) || changed != one.changed {
		t.Fatal("late question changed durable receipt evidence after rebind")
	}
	if err := one.service.AcknowledgeReceipt(t.Context(), "cluster-1", request); err != nil {
		t.Fatalf("late callback stranded the original exact receipt: %v", err)
	}
}

func TestSessionReceiptCannotBeInjectedOrSealPendingEvidence(t *testing.T) {
	one := progressSession(t)
	next := one.copyLocked()
	next.State.ContextID = "native-context"
	command := next.Commands[next.CurrentCommand]
	command.State, command.Settled = nodewire.SessionCommandCompleted, true
	command.Receipt = nodewire.SessionReceipt{Version: 1, Digest: "caller supplied"}
	next.Commands[next.CurrentCommand] = command
	if err := one.commitLocked(next); err == nil {
		t.Fatal("caller-provided digest became durable evidence")
	}
	next = one.copyLocked()
	next.State.ContextID = "native-context"
	command.Receipt = nodewire.SessionReceipt{}
	next.Commands[next.CurrentCommand] = command
	next.State.Questions = []nodewire.SessionQuestion{{ID: "pending", CommandID: next.CurrentCommand, State: nodewire.SessionQuestionPending}}
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	if one.record.Commands[next.CurrentCommand].Receipt.Version != 0 {
		t.Fatal("pending evidence received an acknowledgement key")
	}
	next = one.copyLocked()
	next.State.Questions[0].State = nodewire.SessionQuestionInterrupted
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	if err := one.record.Commands[next.CurrentCommand].Receipt.Validate(); err != nil {
		t.Fatalf("fully settled command did not acquire receipt: %v", err)
	}
}

func TestSealedReceiptRejectsLaterQuestionChanges(t *testing.T) {
	for _, change := range []string{"append", "rewrite"} {
		t.Run(change, func(t *testing.T) {
			one, _, _ := ackFixture(t)
			next := one.copyLocked()
			if change == "append" {
				next.State.Questions = append(next.State.Questions, nodewire.SessionQuestion{
					ID: "late-question", CommandID: next.CurrentCommand, State: nodewire.SessionQuestionInterrupted,
				})
			} else {
				next.State.Questions[0].Question.Title = "changed after seal"
			}
			if err := one.commitLocked(next); err == nil {
				t.Fatal("terminal digest no longer covers retained questions")
			}
		})
	}
}
