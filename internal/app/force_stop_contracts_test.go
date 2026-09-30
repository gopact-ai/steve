package app

import "github.com/gopact-ai/steve/internal/harness"

var _ applicationOpenKiller = (*harness.Manager)(nil)

var _ applicationSessionKiller = (*harness.Manager)(nil)
