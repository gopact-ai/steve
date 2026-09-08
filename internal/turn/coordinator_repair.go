package turn

import (
	"context"

	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Refresher asks a node to check itself again after a repair. The node
// registry satisfies it; the hub's own advert is asked fresh anyway.
type Refresher interface {
	Refresh(ctx context.Context, node string) (nodewire.Advert, error)
}

// MachineFiles reads platform file facts on a machine; "" is the hub.
type MachineFiles interface {
	Files(ctx context.Context, node string, req nodewire.FileRequest) (string, error)
}

// SetRepair wires what the repair verb needs beyond the supervisor.
func (c *Coordinator) SetRepair(nodes Refresher, files MachineFiles) {
	c.refresher = nodes
	c.files = files
}

// SetProber wires model discovery: one endpoint on demand (after a repair,
// the harness that just appeared), and all of them for `/fleet probe`.
func (c *Coordinator) SetProber(one func(ctx context.Context, node, harness string) error, all func(ctx context.Context) []models.Result) {
	c.probeOne = one
	c.probeAll = all
}
