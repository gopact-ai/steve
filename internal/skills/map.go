// Package skills maps which skills Steve exposes to harness runtimes.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type fileData struct {
	SearchPaths []string `json:"search_paths"`
	Enabled     []string `json:"enabled"`
}

type Ref struct {
	Name string
	Path string
}

type Map struct {
	path string
	mu   sync.Mutex
}

func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, "skills.json")
}

func DefaultSearchPath(stateDir string) string {
	return filepath.Join(stateDir, "skills")
}

func Open(path string) (*Map, error) {
	if path == "" {
		return nil, fmt.Errorf("skills map path is required")
	}
	return &Map{path: path}, nil
}

func (m *Map) Ensure(defaultSearch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return err
	}
	if defaultSearch != "" {
		if err := os.MkdirAll(defaultSearch, 0o700); err != nil {
			return fmt.Errorf("create skills directory: %w", err)
		}
		if !contains(data.SearchPaths, defaultSearch) {
			data.SearchPaths = append([]string{defaultSearch}, data.SearchPaths...)
		}
	}
	return m.writeLocked(data)
}

func (m *Map) Enable(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return err
	}
	ref, err := m.resolveLocked(data, name)
	if err != nil {
		return err
	}
	for _, item := range data.Enabled {
		if item == name || filepath.Base(item) == ref.Name {
			return nil
		}
		existing, err := m.resolveLocked(data, item)
		if err == nil && existing.Name == ref.Name {
			return nil
		}
	}
	data.Enabled = append(data.Enabled, name)
	return m.writeLocked(data)
}

func (m *Map) Disable(name string) error {
	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return err
	}
	next := make([]string, 0, len(data.Enabled))
	for _, item := range data.Enabled {
		if enabledMatch(item, name, data, m) {
			continue
		}
		next = append(next, item)
	}
	data.Enabled = next
	return m.writeLocked(data)
}

func (m *Map) AddPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("search path is required")
	}
	resolved, err := abs(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("search path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("search path is not a directory")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return err
	}
	if !contains(data.SearchPaths, resolved) {
		data.SearchPaths = append(data.SearchPaths, resolved)
	}
	return m.writeLocked(data)
}

func (m *Map) RemovePath(path string) error {
	path = strings.TrimSpace(path)
	resolved, _ := abs(path)
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return err
	}
	next := make([]string, 0, len(data.SearchPaths))
	for _, item := range data.SearchPaths {
		if item != path && item != resolved {
			next = append(next, item)
		}
	}
	data.SearchPaths = next
	kept := make([]string, 0, len(data.Enabled))
	for _, item := range data.Enabled {
		if _, err := m.resolveLocked(data, item); err == nil {
			kept = append(kept, item)
		}
	}
	data.Enabled = kept
	return m.writeLocked(data)
}

func (m *Map) Enabled() ([]Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(data.Enabled))
	for _, name := range data.Enabled {
		ref, err := m.resolveLocked(data, name)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, nil
}

func (m *Map) Available() ([]Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var out []Ref
	for _, root := range data.SearchPaths {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			path := filepath.Join(root, entry.Name())
			if !hasSkill(path) {
				continue
			}
			if _, ok := seen[entry.Name()]; ok {
				continue
			}
			seen[entry.Name()] = struct{}{}
			out = append(out, Ref{Name: entry.Name(), Path: path})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Map) SearchPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return nil
	}
	return append([]string{}, data.SearchPaths...)
}

func (m *Map) EnabledNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return nil
	}
	names := append([]string{}, data.Enabled...)
	sort.Strings(names)
	return names
}

func (m *Map) Fingerprint() string {
	enabled, err := m.Enabled()
	var names []string
	if err != nil {
		names = m.EnabledNames()
	} else {
		names = make([]string, 0, len(enabled))
		for _, ref := range enabled {
			names = append(names, ref.Name)
		}
		sort.Strings(names)
	}
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:])
}

func (m *Map) Materialize(dest string) error {
	enabled, err := m.Enabled()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("create runtime skills: %w", err)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dest, entry.Name())); err != nil {
			return err
		}
	}
	for _, ref := range enabled {
		link := filepath.Join(dest, ref.Name)
		if err := os.Symlink(ref.Path, link); err != nil {
			return fmt.Errorf("link skill %q: %w", ref.Name, err)
		}
	}
	return nil
}

func (m *Map) resolveLocked(data fileData, name string) (Ref, error) {
	if filepath.IsAbs(name) || strings.HasPrefix(name, "~/") || name == "~" {
		path, err := abs(name)
		if err != nil {
			return Ref{}, err
		}
		if !hasSkill(path) {
			return Ref{}, fmt.Errorf("not a skill directory: %s", path)
		}
		return Ref{Name: filepath.Base(path), Path: path}, nil
	}
	for _, root := range data.SearchPaths {
		path := filepath.Join(root, name)
		if hasSkill(path) {
			return Ref{Name: name, Path: path}, nil
		}
	}
	return Ref{}, fmt.Errorf("unknown skill %q", name)
}

func (m *Map) readLocked() (fileData, error) {
	raw, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return fileData{}, nil
	}
	if err != nil {
		return fileData{}, fmt.Errorf("read skills map: %w", err)
	}
	var data fileData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fileData{}, fmt.Errorf("parse skills map: %w", err)
	}
	return data, nil
}

func (m *Map) writeLocked(data fileData) error {
	if data.SearchPaths == nil {
		data.SearchPaths = []string{}
	}
	if data.Enabled == nil {
		data.Enabled = []string{}
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".skills-*.json")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, m.path); err != nil {
		os.Remove(name)
		return err
	}
	return syncDir(dir)
}

// syncDir flushes a directory entry after a rename so the replacement
// survives a crash (rename alone is not guaranteed durable on all filesystems).
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func enabledMatch(item, name string, data fileData, m *Map) bool {
	if item == name || filepath.Base(item) == name {
		return true
	}
	if resolved, err := abs(name); err == nil && item == resolved {
		return true
	}
	if ref, err := m.resolveLocked(data, item); err == nil {
		if ref.Name == name || ref.Path == name {
			return true
		}
		if resolved, err := abs(name); err == nil && ref.Path == resolved {
			return true
		}
	}
	return false
}

func hasSkill(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !info.IsDir()
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func abs(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}
