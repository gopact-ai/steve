package console

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

type abandonDriver interface {
	AbandonAttempt(context.Context, string, string, uint64) (attempt.Record, error)
	ProjectAbandoned(context.Context, string) error
	PendingAbandonments(context.Context) ([]attempt.Record, error)
	ReadAbandoned(context.Context, string) (attempt.Record, error)
	CompleteAbandonDelivery(context.Context, attempt.Record, func(ledger.Reader, attempt.Record) (attempt.AbandonDelivery, error)) error
}

var _ consoleapi.Abandons = (*Service)(nil)

func (s *Service) SetAbandons(control abandonDriver) { s.abandons = control }

func (s *Service) Abandon(ctx context.Context, id string, revision uint64) (consoleapi.Abandonment, error) {
	if s.abandons == nil || s.owner == "" {
		return consoleapi.Abandonment{}, errors.New("abandonment requires the console owner")
	}
	if _, err := s.abandons.AbandonAttempt(ctx, id, s.owner, revision); err != nil {
		return consoleapi.Abandonment{}, err
	}
	if err := s.projectAbandoned(ctx, id); err != nil {
		slog.Warn("console: abandonment recorded; projection remains pending", "attempt", id, "error", err)
		return consoleapi.Abandonment{Accepted: true, Pending: true}, nil
	}
	return consoleapi.Abandonment{Accepted: true}, nil
}

func (s *Service) projectAbandoned(ctx context.Context, id string) error {
	if err := s.abandons.ProjectAbandoned(ctx, id); err != nil {
		return err
	}
	r, err := s.abandons.ReadAbandoned(ctx, id)
	if err != nil {
		return err
	}
	if r.Abandoned == nil || r.Abandoned.ProjectedAt.IsZero() {
		return errors.New("abandonment projection is still pending")
	}
	return s.deliverAbandonment(ctx, r)
}

// ReconcileAbandonments completes session retirement independently of process
// exit. It may still be owed after the physical stop was confirmed elsewhere.
func (s *Service) ReconcileAbandonments(parent context.Context) error {
	if s.abandons == nil || !s.abandonMu.TryLock() {
		return nil
	}
	defer s.abandonMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	records, err := s.abandons.PendingAbandonments(ctx)
	if err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	if len(records) == 0 {
		return nil
	}
	start := sort.Search(len(records), func(i int) bool { return records[i].ID > s.abandonAfter })
	var result error
	for i := range min(4, len(records)) {
		r := records[(start+i)%len(records)]
		s.abandonAfter = r.ID
		result = errors.Join(result, s.projectAbandoned(ctx, r.ID))
		if ctx.Err() != nil {
			break
		}
	}
	return result
}
