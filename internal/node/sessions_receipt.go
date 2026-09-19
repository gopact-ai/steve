package node

import (
	"errors"
	"reflect"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// freezeTerminalReceipt runs under the owner lock before the durable commit.
// The digest is never supplied by the caller and never changes on a rebind or
// cleanup. Missing/uncertain native evidence stays retained without a receipt.
func freezeTerminalReceipt(before sessionRecord, next *sessionRecord) error {
	command, exists := next.Commands[next.CurrentCommand]
	if !exists {
		return nil
	}
	old, existed := before.Commands[next.CurrentCommand]
	if command.Receipt != old.Receipt {
		return errors.New("node terminal receipt cannot be supplied or changed by a mutation")
	}
	if old.Receipt.Version != 0 {
		// Closing the process may strengthen cleanup evidence but cannot rewrite
		// a prompt whose result was already offered to the authenticated hub.
		left, right := old, command
		left.ProcessStopped, right.ProcessStopped = false, false
		left.CancelRequested, right.CancelRequested = false, false
		if !reflect.DeepEqual(left, right) {
			return errors.New("node terminal receipt is immutable")
		}
		if !reflect.DeepEqual(receiptQuestions(before.State.Questions, command.ID), receiptQuestions(next.State.Questions, command.ID)) {
			return errors.New("node terminal question evidence is immutable")
		}
		return nil
	}
	if !command.Settled || command.State != nodewire.SessionCommandCompleted && command.State != nodewire.SessionCommandCancelled ||
		next.State.ContextID == "" || existed && before.State.Binding != next.State.Binding {
		return nil
	}
	state := next.State
	state.Command = &command
	state.Questions = nil
	for _, question := range next.State.Questions {
		if question.CommandID != command.ID {
			continue
		}
		if !question.State.Settled() {
			return nil
		}
		state.Questions = append(state.Questions, question)
	}
	receipt, err := nodewire.NewSessionReceipt(state)
	if err != nil {
		return err
	}
	command.Receipt = receipt
	next.Commands[next.CurrentCommand] = command
	return nil
}

func receiptQuestions(questions []nodewire.SessionQuestion, commandID string) []nodewire.SessionQuestion {
	var result []nodewire.SessionQuestion
	for _, question := range questions {
		if question.CommandID == commandID {
			result = append(result, question)
		}
	}
	return result
}
