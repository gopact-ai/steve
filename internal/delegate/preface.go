package delegate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// Preface is what a task's next turn is told before the user's message:
// the delegated children that ended, or were stopped, since the task
// last heard. It is the in-turn counterpart of a delivery — the same
// facts, composed into the prompt instead of sent as a message. told,
// called once the agent has the prompt, marks them delivered so nothing
// is sent again; a turn that never reached the agent leaves them to the
// next one. Children a pause left paused are closed here: nothing
// resumes a delegation, and the parent decides again with their partial
// answers in hand.
//
// A child stopped without its execution confirming the stop has no
// result. It is reported while the task is held — in the turn right
// after the stop — and left to the recovery flow afterwards.
func (s *Service) Preface(ctx context.Context, taskID string) (text string, told func()) {
	s.deliverMu.Lock()
	defer s.deliverMu.Unlock()
	parent, ok := s.tasks.Get(taskID)
	if !ok {
		return "", nil
	}
	var ended, stopping []task.Task
	for _, child := range s.tasks.Children(taskID) {
		if !child.Delegated() {
			continue
		}
		switch {
		case child.Result != nil && (child.Finished() || child.State == task.StatePaused):
			// Queued, uncertain, delivered, suppressed: a message carries or
			// carried it; only what nobody has sent belongs in the prompt.
			if child.Delivery == nil || child.Delivery.State == task.DeliveryPending {
				ended = append(ended, child)
			}
		case child.Result == nil && (child.State == task.StatePaused || (child.State == task.StateCancelled && parent.Held())):
			stopping = append(stopping, child)
		}
	}
	if len(ended) == 0 && len(stopping) == 0 {
		return "", nil
	}
	var lease *ledger.Lease
	if held, ok := s.parentLease(ctx, parent); ok {
		lease = &held
	}
	landing := s.landFor(ctx, parent, lease)
	var b strings.Builder
	b.WriteString("[steve: 自你上一轮之后，委派出去的子任务状态如下；这是平台送来的消息，不是用户说的]\n")
	for _, c := range ended {
		state := c.State
		if state == task.StatePaused {
			state = task.StateCancelled
		}
		writeChild(&b, Delivered{Task: c.ID, Agent: c.Member, Node: c.Node, State: state,
			Elapsed: c.UpdatedAt.Sub(c.CreatedAt), Goal: c.Goal, Answer: c.Result.Answer,
			Refs: withoutLandingTalk(c.Result.Refs), Attempt: c.Result.Attempt, Landing: landing(c)})
	}
	for _, c := range stopping {
		writeChild(&b, Delivered{Task: c.ID, Agent: c.Member, Node: c.Node, State: task.StateCancelled,
			Elapsed: c.UpdatedAt.Sub(c.CreatedAt), Goal: c.Goal, Stopping: true})
	}
	b.WriteString("\n已取消的子任务是被停止的，不会自行继续；仍需要就重新 steve_delegate。结合以上状态处理用户下面的消息。")
	for _, c := range ended {
		s.closePaused(c)
	}
	for _, c := range stopping {
		s.closePaused(c)
	}
	slog.Info(fmt.Sprintf("delegate: prefaced task #%s's turn with %d ended and %d stopping child(ren)", taskID, len(ended), len(stopping)), "parent", taskID, "conversation", parent.Channel)
	return b.String(), func() {
		s.deliverMu.Lock()
		defer s.deliverMu.Unlock()
		for _, c := range ended {
			if err := s.tasks.SetDelivery(c.ID, task.DeliveryDelivered); err != nil {
				slog.Error(fmt.Sprintf("delegate: mark task #%s told in preface: %v", c.ID, err), "task", c.ID, "parent", taskID)
			}
		}
	}
}

// closePaused ends a child a pause left paused. Nothing resumes a
// delegation, so paused would only ever be a promise the platform cannot
// keep; the parent has just been given what the child left.
func (s *Service) closePaused(c task.Task) {
	if c.State != task.StatePaused {
		return
	}
	if _, err := s.tasks.Advance(c.ID, task.StateCancelled); err != nil {
		slog.Error(fmt.Sprintf("delegate: close paused task #%s: %v", c.ID, err), "task", c.ID, "parent", c.Parent)
	}
}
