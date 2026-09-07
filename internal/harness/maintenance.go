package harness

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
)

// SuspendNode closes cached hosts after the caller has sealed execution
// admission and verified idle. The block also covers background probes.
func (m *Manager) SuspendNode(ctx context.Context, node string, all bool) (func(), error) {
	m.mu.Lock()
	if m.suspended == nil {
		m.suspended = map[string]bool{}
	}
	key := node
	if all {
		key = "*"
	}
	m.suspended[key] = true
	hosts := map[string]*acphost.Host{}
	for name, host := range m.hosts {
		matches := all || node == "" && !strings.Contains(name, "/") || node != "" && strings.HasPrefix(name, node+"/")
		if matches {
			hosts[name] = host
		}
	}
	m.mu.Unlock()
	for _, host := range hosts {
		host.Close()
	}
	release := func() { m.mu.Lock(); defer m.mu.Unlock(); delete(m.suspended, key) }
	for name, host := range hosts {
		for !host.AllProcessesStopped() {
			select {
			case <-ctx.Done():
				return release, fmt.Errorf("cached agent process exit is unconfirmed: %w", ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
		m.mu.Lock()
		if m.hosts[name] == host {
			delete(m.hosts, name)
		}
		m.mu.Unlock()
	}
	return release, nil
}
