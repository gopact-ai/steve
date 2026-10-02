package consoleapi

import "context"

// Abandonment distinguishes an accepted accounting decision from completion of
// its session projection. Neither outcome claims that the original writer died.
type Abandonment struct {
	Accepted bool `json:"accepted"`
	Pending  bool `json:"pending"`
}

type Abandons interface {
	Abandon(context.Context, string, uint64) (Abandonment, error)
}
