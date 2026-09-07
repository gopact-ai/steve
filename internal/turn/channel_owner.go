package turn

import (
	"errors"
	"fmt"
	"strings"
)

// SetChannelOwner registers the trusted adapter's native owner at startup.
// Console/internal calls keep SetIdentity's baseline; sender IDs are never
// rewritten because tasks, notices and callbacks use their native identity.
func (c *Coordinator) SetChannelOwner(channel, owner string) error {
	if channel == "" || channel == "console" || strings.TrimSpace(channel) != channel {
		return errors.New("a non-console channel is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channelOwners == nil {
		c.channelOwners = map[string]string{}
	}
	c.channelOwners[channel] = owner
	return nil
}

func (c *Coordinator) forChannel(channel string) (*Coordinator, error) {
	if channel == "" || channel == "console" {
		return c, nil
	}
	c.mu.Lock()
	owner, ok := c.channelOwners[channel]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unregistered request channel %q", channel)
	}
	return &Coordinator{coordinatorState: c.coordinatorState, text: c.text, ownerOpenID: owner}, nil
}
