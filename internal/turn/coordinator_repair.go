package turn

import (
	"context"

	"github.com/gopact-ai/steve/internal/models"
)

// SetProber wires model discovery: one endpoint on demand (after a repair,
// the harness that just appeared), and all of them for `/fleet probe`.
func (c *Coordinator) SetProber(one func(ctx context.Context, node, harness string) error, all func(ctx context.Context) []models.Result) {
	c.probeOne = one
	c.probeAll = all
}
