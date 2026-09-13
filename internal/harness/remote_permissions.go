package harness

import (
	"fmt"
	"maps"

	"github.com/gopact-ai/steve/internal/permission"
)

// SetRemotePermissions installs coordinator-owned execution policy without
// registering a local command. Existing processes retain their original policy.
func (m *Manager) SetRemotePermissions(policies map[string]string) error {
	for id, policy := range policies {
		if id == "" {
			return fmt.Errorf("remote permission needs a harness id")
		}
		if _, err := permission.New(policy); err != nil {
			return fmt.Errorf("remote harness %s: %w", id, err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remotePermissions = maps.Clone(policies)
	return nil
}

// remotePermission is read with the manager mutex held.
func (m *Manager) remotePermission(id string) string {
	if policy, ok := m.remotePermissions[id]; ok {
		return policy
	}
	if policy := m.configs[id].Permission; policy != "" {
		return policy
	}
	return permission.PolicyRead
}
