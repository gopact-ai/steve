package skills

import (
	"fmt"
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

func Setup(stateDir string) (*Map, error) {
	m, err := Open(DefaultPath(stateDir))
	if err != nil {
		return nil, err
	}
	if err := m.Ensure(DefaultSearchPath(stateDir)); err != nil {
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

func (l *Live) Apply() error {
	if l == nil || l.Map == nil {
		return fmt.Errorf("skills map is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.applyLocked()
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
