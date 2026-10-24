package config

import (
	"fmt"
	"strings"
)

// ValidateChannels checks the owner identity every channel answers to. The
// console is the only channel; it needs an owner before anyone can use it.
func (c *Config) ValidateChannels() error {
	if strings.TrimSpace(c.EffectiveOwnerID()) == "" {
		return fmt.Errorf("gateway.owner_id is required")
	}
	return nil
}
