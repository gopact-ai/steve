package admin

import (
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestObservedHubAdvertToleratesAnUnprobedObservation(t *testing.T) {
	// An observation built without its launch probe must still describe
	// the hub's machine, only without the launch results.
	adv := ObservedHubAdvert("node-hub", NewConfigStore(&config.Config{}), &LocalObservation{})
	if adv.Snapshot == nil {
		t.Fatalf("advert without snapshot: %+v", adv)
	}
}

// A store with no configuration, a nil one included, gives the hub an
// advert that names the node and nothing else.
func TestObservedHubAdvertWithoutConfigurationNamesOnlyTheNode(t *testing.T) {
	for name, store := range map[string]*ConfigStore{"nil store": nil, "empty store": NewConfigStore(nil)} {
		t.Run(name, func(t *testing.T) {
			adv := ObservedHubAdvert("node-hub", store, &LocalObservation{})
			if want := (nodewire.Advert{Node: "node-hub"}); !reflect.DeepEqual(adv, want) {
				t.Fatalf("advert without configuration = %+v, want %+v", adv, want)
			}
		})
	}
}
