package turn

import (
	"fmt"
	"strings"
)

// channelOwners is who owns a request by the channel it arrives on: the
// baseline owner for console and internal calls, and each trusted adapter's
// native owner for its own channel. Sender IDs are never rewritten because
// tasks, notices and callbacks use their native identity.
type channelOwners struct {
	baseline  string
	byChannel map[string]string
}

// newChannelOwners copies each adapter's owner, refusing a console or
// malformed channel.
func newChannelOwners(baseline string, owners map[string]string) (channelOwners, error) {
	registered := make(map[string]string, len(owners))
	for channel, owner := range owners {
		if channel == "" || channel == "console" || strings.TrimSpace(channel) != channel {
			return channelOwners{}, fmt.Errorf("owner of channel %q: a non-console channel is required", channel)
		}
		registered[channel] = owner
	}
	return channelOwners{baseline: baseline, byChannel: registered}, nil
}

// of is the owner a request from channel runs as. The owners are fixed at
// construction, so the lookup needs no lock.
func (o channelOwners) of(channel string) (string, error) {
	if channel == "" || channel == "console" {
		return o.baseline, nil
	}
	owner, ok := o.byChannel[channel]
	if !ok {
		return "", fmt.Errorf("unregistered request channel %q", channel)
	}
	return owner, nil
}

// forChannel is the view a request from channel runs as. The owner is always
// resolved from channel alone, whatever owner the receiver view carries.
func (c *Coordinator) forChannel(channel string) (*Coordinator, error) {
	owner, err := c.owners.of(channel)
	if err != nil {
		return nil, err
	}
	if owner == c.ownerOpenID {
		return c, nil
	}
	return &Coordinator{coordinatorState: c.coordinatorState, text: c.text, ownerOpenID: owner}, nil
}
