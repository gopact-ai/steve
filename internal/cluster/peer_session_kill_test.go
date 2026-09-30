package cluster

import (
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestKillIsAuthorizedAsStopping(t *testing.T) {
	observation, stopping, err := sessionActionMode(nodewire.SessionActionKill)
	if err != nil || observation || !stopping {
		t.Fatalf("kill scope=%v %v %v", observation, stopping, err)
	}
}
