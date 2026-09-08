package admin

import (
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

func TestObservedHubAdvertToleratesAnUnprobedObservation(t *testing.T) {
	// An observation built without its launch probe must still describe
	// the hub's machine, only without the launch results.
	adv := ObservedHubAdvert(&config.Config{}, &LocalObservation{})
	if adv.Snapshot == nil {
		t.Fatalf("advert without snapshot: %+v", adv)
	}
}
