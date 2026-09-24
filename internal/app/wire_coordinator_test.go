package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

type stubGate struct{}

func (stubGate) PrepareExtras(string, string, string, string) ([]capability.Extra, error) {
	return nil, nil
}
func (stubGate) DescribeExtras(string, string) []capability.Extra { return nil }

// coordinatorCallbacks takes every turn.Callbacks field from one of its
// inputs, so a field added to Callbacks without a source fails here instead
// of being wired as nil. Only the mapping is checked: every input here is
// set, so this says nothing about whether each stage builds a value. Wire
// panics when a callback it requires is missing.
func TestCoordinatorCallbacksFillEveryField(t *testing.T) {
	got := coordinatorCallbacks(turntest.IdleSupervisor{},
		func(context.Context, string, string) error { return nil },
		messagingCallbacks{
			AgentGate:   stubGate{},
			AfterTurn:   func(string) {},
			TurnPreface: func(context.Context, string) turn.Preface { return turn.Preface{} },
		},
		taskRoutes{
			Notifier:         func(turn.TaskNotice) {},
			Resumer:          func(turn.TaskResume) error { return nil },
			ResumeDispatcher: func(turn.TaskResume) {},
		})
	value := reflect.ValueOf(got)
	for i := range value.NumField() {
		if value.Field(i).IsZero() {
			t.Errorf("Callbacks.%s is not filled", value.Type().Field(i).Name)
		}
	}
}
