// Package identity resolves authenticated callers in their channel's namespace.
// It consumes the configured authority; it does not persist a second owner store.
package identity

import (
	"fmt"
	"maps"
	"strings"
)

// Configuration is supplied by the composition root. NativeOwners contains only
// explicitly trusted non-Console adapters. The zero value has no owner authority.
type Configuration struct {
	ConsoleOwner string
	NativeOwners map[string]string
}

// Owners is an immutable snapshot of the configured channel identities. Native
// IDs remain unchanged in tasks, receipts and grants; equal strings in different
// channels do not establish a cross-channel principal binding.
type Owners struct{ config Configuration }

func NewOwners(config Configuration) (Owners, error) {
	for channel := range config.NativeOwners {
		if channel == "" || channel == "console" || strings.TrimSpace(channel) != channel {
			return Owners{}, fmt.Errorf("owner of channel %q: a non-console channel is required", channel)
		}
	}
	config.NativeOwners = maps.Clone(config.NativeOwners)
	return Owners{config: config}, nil
}

// Owner resolves only a registered channel. Empty names retain the existing
// trusted internal-call convention and use the Console baseline.
func (o Owners) Owner(channel string) (string, error) {
	if channel == "" || channel == "console" {
		return o.config.ConsoleOwner, nil
	}
	owner, ok := o.config.NativeOwners[channel]
	if !ok {
		return "", fmt.Errorf("unregistered request channel %q", channel)
	}
	return owner, nil
}

// Configuration returns an independent copy for another consumer of the same
// authority, without allowing either consumer to change this snapshot.
func (o Owners) Configuration() Configuration {
	config := o.config
	config.NativeOwners = maps.Clone(config.NativeOwners)
	return config
}

func (o Owners) ConsoleOwner() string { return o.config.ConsoleOwner }
