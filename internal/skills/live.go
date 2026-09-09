package skills

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// Live applies a map onto isolated harness skill directories and can
// restart harness processes so they rescan the materialized links.
type Live struct {
	Map   *Map
	Dests []string
	After func() error

	mu sync.Mutex
}

// Setup opens the map, makes sure the user's own skills directory is
// there, and brings the shipped skills up to date: written fresh under
// the state directory, listed last, on by default the first time each
// is seen.
func Setup(stateDir string) (*Map, error) {
	m, err := Open(DefaultPath(stateDir))
	if err != nil {
		return nil, err
	}
	if err := m.Ensure(DefaultSearchPath(stateDir)); err != nil {
		return nil, err
	}
	root, err := InstallBuiltins(stateDir)
	if err != nil {
		return nil, err
	}
	names, err := BuiltinNames()
	if err != nil {
		return nil, err
	}
	if err := m.EnsureBuiltins(root, names); err != nil {
		return nil, err
	}
	return m, nil
}

func (l *Live) Enable(name string) error {
	return l.mutate(func() error { return l.Map.Enable(name) })
}

func (l *Live) Disable(name string) error {
	return l.mutate(func() error { return l.Map.Disable(name) })
}

func (l *Live) AddPath(path string) error {
	if l == nil || l.Map == nil {
		return fmt.Errorf("skills map is not configured")
	}
	return l.Map.AddPath(path)
}

func (l *Live) RemovePath(path string) error {
	return l.mutate(func() error { return l.Map.RemovePath(path) })
}

// AddSource installs a git source. Nothing is enabled by it, so nothing
// restarts.
func (l *Live) AddSource(ctx context.Context, spec string) (Source, error) {
	if l == nil || l.Map == nil {
		return Source{}, fmt.Errorf("skills map is not configured")
	}
	return l.Map.AddSource(ctx, spec)
}

// UpdateSources fetches every source again. The text of an enabled
// skill may have changed, so what the machines hold is repacked and the
// AI tools restart, whether or not the enabled set moved.
func (l *Live) UpdateSources(ctx context.Context) ([]Source, error) {
	if l == nil || l.Map == nil {
		return nil, fmt.Errorf("skills map is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out, err := l.Map.UpdateSources(ctx)
	if err != nil {
		return out, err
	}
	return out, l.applyLocked()
}

// RemoveSource forgets a source; skills enabled from it go with it.
func (l *Live) RemoveSource(slug string) error {
	return l.mutate(func() error { return l.Map.RemoveSource(slug) })
}

func (l *Live) Apply() error {
	if l == nil || l.Map == nil {
		return fmt.Errorf("skills map is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.applyLocked()
}

// AddDests prepares newly registered runtimes for subsequent skill changes.
// It does not restart existing agents when a different tool is registered.
func (l *Live) AddDests(dests ...string) error {
	if l == nil || l.Map == nil {
		return fmt.Errorf("skills map is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var added []string
	for _, dest := range dests {
		if slices.Contains(l.Dests, dest) || slices.Contains(added, dest) {
			continue
		}
		if err := l.Map.Materialize(dest); err != nil {
			return err
		}
		added = append(added, dest)
	}
	l.Dests = append(l.Dests, added...)
	return nil
}

func (l *Live) mutate(op func() error) error {
	if l == nil || l.Map == nil {
		return fmt.Errorf("skills map is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	before := l.Map.Fingerprint()
	if err := op(); err != nil {
		return err
	}
	if l.Map.Fingerprint() == before {
		return nil
	}
	return l.applyLocked()
}

func (l *Live) applyLocked() error {
	for _, dest := range l.Dests {
		if err := l.Map.Materialize(dest); err != nil {
			return err
		}
	}
	if l.After == nil {
		return nil
	}
	if err := l.After(); err != nil {
		return fmt.Errorf("restart harnesses: %w", err)
	}
	return nil
}
