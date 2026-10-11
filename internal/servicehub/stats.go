package servicehub

import (
	"context"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/interaction"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/work"
)

type Stats = durable.ServiceStats

func (s *Services) Stats(ctx context.Context) (out Stats, err error) {
	out.Queues = map[string]durable.QueueStats{}
	out.Limits = map[string]durable.Limits{"events": s.Events.Limits.Effective(), "deliveries": s.Events.Limits.Effective(), "client_invocations": s.Clients.Limits.Effective(), "work": s.Work.Limits.Effective()}
	after := s.Retention.After
	if after <= 0 {
		after = 7 * 24 * time.Hour
	}
	out.RetentionDays = int(after / (24 * time.Hour))
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		for _, queue := range []struct{ name, index, kind, at string }{
			{"event_outbox", events.OutboxKind, events.EventKind, "$.at"},
			{"event_inbox", events.InboxOpenKind, events.InboxKind, "$.at"},
			{"client_attention", clientcap.AttentionKind, clientcap.CallKind, "$.at"},
		} {
			stats, err := durable.QueueStatsTx(tx, queue.index, queue.kind, queue.at)
			if err != nil {
				return err
			}
			out.Queues[queue.name] = stats
		}
		workStats, err := work.BacklogTx(tx)
		if err != nil {
			return err
		}
		out.Queues["work"] = workStats
		for _, queue := range []struct {
			name, kind, at string
			states         []string
		}{
			{"event_pending", events.DeliveryKind, "$.updated_at", []string{events.StatePending}},
			{"event_claimed", events.DeliveryKind, "$.updated_at", []string{events.StateClaimed}},
			{"event_accepted", events.DeliveryKind, "$.updated_at", []string{events.StateAccepted}},
			{"event_unknown", events.DeliveryKind, "$.updated_at", []string{events.StateUnknown}},
			{"client_pending", clientcap.CallKind, "$.at", []string{clientcap.StatePending}},
			{"client_approval", clientcap.CallKind, "$.at", []string{clientcap.StateApproval}},
			{"client_claimed", clientcap.CallKind, "$.at", []string{clientcap.StateClaimed}},
			{"client_unknown", clientcap.CallKind, "$.at", []string{clientcap.StateUnknown}},
			{"interaction_pending", interaction.RecordKind, "$.created_at", []string{"pending"}},
		} {
			var stats durable.QueueStats
			marks := make([]string, len(queue.states))
			args := []any{queue.at, queue.kind}
			for i, state := range queue.states {
				marks[i] = "?"
				args = append(args, state)
			}
			if err := tx.QueryRow(`SELECT count(*),coalesce(min(json_extract(data,?)),'') FROM bindings WHERE kind=? AND (`+durable.StateExpression+`) IN (`+strings.Join(marks, ",")+`)`, args...).Scan(&stats.Pending, &stats.Oldest); err != nil {
				return err
			}
			out.Queues[queue.name] = stats
		}
		if err := tx.QueryRow(`SELECT coalesce(sum(length(data)),0) FROM bindings WHERE kind IN (?,?,?,?,?,?)`, events.EventKind, events.DeliveryKind, work.RecordKind, work.InputKind, interaction.RecordKind, clientcap.CallKind).Scan(&out.Storage.HotBytes); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT count(*),coalesce(sum(length(data)),0),coalesce(sum(json_extract(data,'$.bytes')),0) FROM bindings WHERE kind=?`, durable.ColdKind).Scan(&out.Storage.ColdRecords, &out.Storage.ColdBytes, &out.Storage.ColdOriginalBytes)
	})
	return
}
