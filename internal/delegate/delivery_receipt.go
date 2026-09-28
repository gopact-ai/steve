package delegate

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
)

func (s *Service) currentWaiting(candidates []task.Task) []task.Task {
	out := candidates[:0]
	for _, candidate := range candidates {
		t, ok := s.tasks.Get(candidate.ID)
		if !ok || !t.Delegated() || !t.Finished() || t.Result == nil {
			continue
		}
		if d := t.Delivery; d != nil && (d.State == task.DeliveryDelivered || d.State == task.DeliverySuppressed) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Inspect an existing queue entry before applying parent-state gates or
// rebuilding landing descriptions. This path never starts a parent turn.
// A continuation without a settled receipt once its parent has ended is
// suppressed, with the reason in the Hub's language: delegate sends the
// ended parent nothing more and stops waiting on that entry, which stays
// in the conversation and is not sent again. Whatever the conversation
// later does with it does not change the child's record.
func (s *Service) checkDeliveryReceipts(parent task.Task, waiting []task.Task, receipt func(task.Task, string) (bool, error)) []task.Task {
	groups := map[string][]string{}
	for _, child := range waiting {
		if d := child.Delivery; d != nil && d.Key != "" {
			groups[d.Key] = append(groups[d.Key], child.ID)
		}
	}
	observed := map[string]bool{}
	for key, ids := range groups {
		found, err := receipt(parent, key)
		if !found {
			continue
		}
		// Reserve this batch from dispatch even if the task receipt save fails.
		// Nothing is acknowledged in the store until RecordDelivery succeeds;
		// the next pass rereads the retained, non-consuming queue receipt.
		observed[key] = true
		state, detail := task.DeliveryDelivered, ""
		if errors.Is(err, channel.ErrDeliveryQueued) {
			state = task.DeliveryQueued
			if parent.State.Terminal() {
				state, detail = task.DeliverySuppressed, s.text.T(i18n.DelegateSuppressedUnconfirmed, parent.ID, noticeState(s.text, parent.State))
			}
		} else if err != nil {
			state, detail = task.DeliveryUncertain, err.Error()
		}
		if err := s.tasks.RecordDelivery(ids, state, detail); err != nil {
			slog.Error("delegate: save durable queue receipt", "parent", parent.ID, "error", err)
		} else if state == task.DeliverySuppressed {
			slog.Info(fmt.Sprintf("delegate: suppressed %d child result(s) for ended task #%s: continuation not settled", len(ids), parent.ID), "parent", parent.ID, "key", key, "conversation", parent.Channel)
		}
	}
	out := waiting[:0]
	for _, child := range waiting {
		if child.Delivery == nil || !observed[child.Delivery.Key] {
			out = append(out, child)
		}
	}
	return out
}
