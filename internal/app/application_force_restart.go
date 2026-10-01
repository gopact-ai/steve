package app

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
)

func (s *applicationStops) restartForceStop(ctx context.Context, r attempt.Record) error {
	return errors.New("member restart is unavailable")
}
