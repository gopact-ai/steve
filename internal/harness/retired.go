package harness

import (
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/pihome"
)

// retired are built-in harnesses Steve used to support. An installation
// may still name them in its configuration or on a machine; it starts
// normally, and those agents are shown as no longer supported and never
// start new work until their owner removes them.
var retired = map[string]string{
	"kimi": "Kimi Code",
	"grok": "Grok",
	"dsh":  "DeepSeek Harness",
}

// ErrRetired is a harness Steve no longer supports.
var ErrRetired = errors.New("agent tool is no longer supported")

// Retired reports whether id is a harness Steve no longer supports, and the
// tool's name.
func Retired(id string) (string, bool) {
	name, ok := retired[id]
	return name, ok
}

// RetiredError says which tool is no longer supported and what to do.
func RetiredError(id string) error {
	name, ok := Retired(id)
	if !ok {
		return nil
	}
	return fmt.Errorf("%w: %s (%s); Steve supports Codex, Claude Code and pi — remove agents that use it", ErrRetired, name, id)
}

// envForPolicy fits a local launch to its permission policy. pi asks for no
// approval, so a read-only policy is held by starting it from a home that
// loads no write or shell tools, and refused where that cannot be shown.
func envForPolicy(env []string, harnessID, policy string) ([]string, error) {
	if harnessID != Pi {
		return env, nil
	}
	return pihome.Launch(env, policy)
}

// ContextResetPolicy is a resume Steve started afresh because the session
// was opened under another permission class: pi would restore the tools
// recorded in it.
const ContextResetPolicy = "permission_class_changed"

// ContextResetter is a Runner that can report it started a new native
// session in place of the one asked for.
type ContextResetter interface {
	ContextReset() string
}

// localPi is the environment of a pi started on this host, and whether at
// is one.
func (m *Manager) localPi(at Placement) ([]string, bool) {
	if at.Harness != Pi || at.Node != "" {
		return nil, false
	}
	m.mu.Lock()
	cfg := m.configs[at.Harness]
	m.mu.Unlock()
	return cfg.Env, true
}
