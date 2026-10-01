package console

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

type abandonDriver interface {
	AbandonAttempt(context.Context, string, string, uint64) (attempt.Record, error)
	ProjectAbandoned(context.Context, string) error
	PendingAbandonments(context.Context) ([]attempt.Record, error)
	ReadAbandoned(context.Context, string) (attempt.Record, error)
}

func (s *Service) SetAbandons(control abandonDriver) { s.abandons = control }
func (s *Service) Abandon(ctx context.Context, id string, revision uint64) (consoleapi.Abandonment, error) {
	return consoleapi.Abandonment{}, errors.New("abandonment is unavailable")
}
