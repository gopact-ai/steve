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
func CheckHealth(workspaceRoot, stateDir string) *nodewire.Health {
	where := workspaceRoot
	if where == "" {
		where = stateDir
	}
	if where == "" {
		where = "."
	}
	h := &nodewire.Health{At: time.Now().UTC(), Load1: loadOne()}
	h.DiskFree, h.DiskTotal = diskOf(where)
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
