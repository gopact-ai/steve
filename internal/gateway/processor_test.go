package gateway

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// The idle coordinator offers every capability the gateway probes its
// processor for, so a fake that embeds it answers through the same calls
// the coordinator does.
var (
	_ inputParser        = turntest.IdleCoordinator{}
	_ scheduledValidator = turntest.IdleCoordinator{}
)

// plainProcessor runs turns and offers nothing else.
type plainProcessor struct{}

func (plainProcessor) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, nil
}

// idleProcessor is the idle coordinator with turns that answer nothing.
type idleProcessor struct{ turntest.IdleCoordinator }

func (idleProcessor) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, nil
}

// A gateway over a coordinator that knows nothing classifies a line by its
// syntax alone and fires a schedule without checking it first. The idle
// coordinator is answered the way a processor that offers only Handle is.
func TestGatewayOverACoordinatorThatKnowsNothing(t *testing.T) {
	for name, p := range map[string]processor{"plain": plainProcessor{}, "idle": idleProcessor{}} {
		t.Run(name, func(t *testing.T) {
			g := New(p)
			for _, line := range []string{"hello", "!now", "/cancel", "@builder /cancel", "/use builder /cancel", "@builder/cancel", "@builder /every 30m inspect", "/schedules"} {
				_, parsed := turn.ParseAddressedInput(line)
				if got, want := g.immediateInput(line), parsed.Interrupt || parsed.Control(); got != want {
					t.Errorf("%q immediate = %v; want %v", line, got, want)
				}
				if got, want := g.scheduleControl(line), parsed.ScheduleControl(); got != want {
					t.Errorf("%q schedule control = %v; want %v", line, got, want)
				}
			}
			g.BindChannel(&scheduledNotice{id: "notice"})
			if receipt, err := g.FireSchedule(t.Context(), testFire()); err != nil || receipt.MessageID != "notice" {
				t.Fatalf("fire = %+v, %v; want it announced and run", receipt, err)
			}
		})
	}
}
