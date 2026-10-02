package console

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/turn"
)

type abandonmentLookupGate struct {
	*recoveryDriver
	entered chan struct{}
	release chan struct{}
}

func (d *abandonmentLookupGate) RetainedChatsFor(ctx context.Context, conversation, message string) ([]turn.RetainedChat, error) {
	close(d.entered)
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.recoveryDriver.RetainedChatsFor(ctx, conversation, message)
}

func TestLateRecoveryCannotReopenADeliveredAbandonment(t *testing.T) {
	for _, duringLookup := range []bool{false, true} {
		t.Run(map[bool]string{false: "after delivery", true: "lookup in flight"}[duringLookup], func(t *testing.T) {
			s, _, _, r := durableAbandonFixture(t)
			driver := &abandonmentLookupGate{recoveryDriver: &recoveryDriver{candidates: []turn.RetainedChat{{AttemptID: r.ID, TaskID: r.TaskID, Conversation: "console:delivery", MessageID: r.TurnID}}}, entered: make(chan struct{}), release: make(chan struct{})}
			s.recoveryDriver = driver
			s.mu.Lock()
			target := s.exchanges["console:delivery"][0]
			ctx, cancel := context.WithCancel(t.Context())
			target.ctx, target.cancel = ctx, cancel
			s.mu.Unlock()
			defer cancel()
			type result struct {
				continued bool
				panicText string
			}
			done := make(chan result, 1)
			resume := func() {
				out := result{}
				defer func() {
					if p := recover(); p != nil {
						out.panicText = fmt.Sprint(p)
					}
					done <- out
				}()
				out.continued = s.continueDetached(target, harness.ErrStopUnconfirmed)
			}
			if duringLookup {
				go resume()
				select {
				case <-driver.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("retained lookup did not begin")
				}
			}
			if out, err := s.Abandon(t.Context(), r.ID, 1); err != nil || out.Pending {
				t.Fatalf("delivery=%+v %v", out, err)
			}
			if duringLookup {
				close(driver.release)
			} else {
				close(driver.release)
				go resume()
			}
			select {
			case out := <-done:
				if out.panicText != "" || out.continued {
					t.Fatalf("late worker touched terminal abandonment: continued=%v panic=%s", out.continued, out.panicText)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("late worker did not leave a terminal abandonment")
			}
			if len(s.Replies("delivery")) != 1 {
				t.Fatal("late worker delivered another abandonment reply")
			}
		})
	}
}
