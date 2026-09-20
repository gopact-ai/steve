package skills

import (
	"cmp"
	"context"
	"errors"
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

	mu      sync.Mutex
	applied liveState
	// Only committed removals awaiting a successful apply may be retried
	// after their source record has disappeared.
	pendingRemovals map[string]bool
}

// The bundle determines whether harnesses must rescan; both logical and
// physical source paths determine whether their links need preparing.
// An empty hash means no successful application has established a baseline.
type liveState struct {
	hash     string
	refs     []Ref
	resolved []Ref
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

// UpdateSources fetches every source again, preparing changed links and
// restarting harnesses only when the enabled bundle changed or applying
// it previously failed.
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
	return l.mutate(func() error {
		if l.pendingRemovals[slug] {
			l.Map.mu.Lock()
			data, err := l.Map.readLocked()
			l.Map.mu.Unlock()
			if err != nil {
				return err
			}
			// A source reinstalled under the same slug must be removed anew.
			if !slices.ContainsFunc(data.Sources, func(s Source) bool { return s.Slug == slug }) {
				return nil
			}
		}
		err := l.Map.RemoveSource(slug)
		var committed *committedWriteError
		if err != nil && !errors.As(err, &committed) {
			return err
		}
		if l.pendingRemovals == nil {
			l.pendingRemovals = make(map[string]bool)
		}
		l.pendingRemovals[slug] = true
		if err != nil {
			// Rename already published the deletion. Preserve its durability
			// error, but allow the missing source to reconcile on retry.
			l.applied = liveState{}
		}
		return err
	})
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
	if err := op(); err != nil {
		return err
	}
	return l.applyLocked()
}

func (l *Live) applyLocked() error {
	desired, err := l.desiredLocked()
	if err != nil {
		return err
	}
	contentChanged := desired.hash != l.applied.hash
	if !contentChanged && slices.Equal(desired.refs, l.applied.refs) && slices.Equal(desired.resolved, l.applied.resolved) {
		l.pendingRemovals = nil
		return nil
	}
	// Preparation and After can each fail after partial side effects. Do not
	// call even the previous bundle applied until reconciliation succeeds.
	l.applied = liveState{}
	for _, dest := range l.Dests {
		if err := l.Map.Materialize(dest); err != nil {
			return err
		}
	}
	if contentChanged && l.After != nil {
		if err := l.After(); err != nil {
			return fmt.Errorf("apply skills: %w", err)
		}
	}
	// Startup prepares before After is wired. That preparation establishes
	// the baseline too: wiring the callback alone is not a content change.
	l.applied = desired
	l.pendingRemovals = nil
	return nil
}

func (l *Live) desiredLocked() (liveState, error) {
	refs, err := l.Map.Enabled()
	if err != nil {
		return liveState{}, err
	}
	slices.SortFunc(refs, func(a, b Ref) int { return cmp.Compare(a.Name, b.Name) })
	resolved, err := ResolveRefs(refs)
	if err != nil {
		return liveState{}, err
	}
	bundle, err := Pack(resolved)
	if err != nil {
		return liveState{}, fmt.Errorf("pack enabled skills: %w", err)
	}
	return liveState{hash: bundle.Hash, refs: refs, resolved: resolved}, nil
}
