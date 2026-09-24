package turn

import (
	"fmt"
	"strings"
)

// channelOwners copies each trusted adapter's native owner, refusing a
// console or malformed channel. Console and internal calls keep the
// baseline owner; sender IDs are never rewritten because tasks, notices and
// callbacks use their native identity.
func channelOwners(owners map[string]string) (map[string]string, error) {
	registered := make(map[string]string, len(owners))
	for channel, owner := range owners {
		if channel == "" || channel == "console" || strings.TrimSpace(channel) != channel {
			return nil, fmt.Errorf("owner of channel %q: a non-console channel is required", channel)
		}
		registered[channel] = owner
	}
	return registered, nil
}

// forChannel is the view a request from channel runs as. The owners are
// fixed at New, so the lookup needs no lock.
func (c *Coordinator) forChannel(channel string) (*Coordinator, error) {
	if channel == "" || channel == "console" {
		return c, nil
	}
	owner, ok := c.channelOwners[channel]
	if !ok {
		return nil, fmt.Errorf("unregistered request channel %q", channel)
	}
	return &Coordinator{coordinatorState: c.coordinatorState, text: c.text, ownerOpenID: owner}, nil
}
