package node

import (
	"os"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// CheckHealth is what a machine can say about its room to work: free
// disk where workspaces live, the one-minute load, and how many
// worktrees it is holding. It is capacity, not capability — a separate
// fact from the snapshot, refreshed with every advert.
// ownSpace is this machine's measurement of Steve's own directories;
// CheckHealth reads it and the walk behind it runs at most this often.
var ownSpace Space

const spaceFreshness = 10 * time.Minute

func CheckHealth(workspaceRoot, stateDir string) *nodewire.Health {
	where := workspaceRoot
	if where == "" {
		where = stateDir
	}
	if where == "" {
		where = "."
	}
	h := &nodewire.Health{At: time.Now().UTC(), Load1: loadOne(), Root: workspaceRoot}
	h.DiskFree, h.DiskTotal = diskOf(where)
	// What Steve holds here is measured in the background: this advert
	// carries the most recent walk rather than waiting for a new one.
	space := ownSpace.Get(workspaceRoot, stateDir, spaceFreshness)
	h.WorkspaceBytes, h.StateBytes, h.SpaceAt, h.SpacePartial = space.workspace, space.state, space.at, space.partial
	if workspaceRoot != "" {
		if entries, err := os.ReadDir(workspaceRoot); err == nil {
			for _, e := range entries {
				if e.IsDir() && strings.HasPrefix(e.Name(), "wt-") {
					h.Worktrees++
				}
			}
		}
	}
	return h
}
