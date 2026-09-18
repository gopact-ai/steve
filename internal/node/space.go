package node

import (
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Space is what Steve costs on this machine: the bytes under the workspace
// it was given and under the directory it keeps its own state in. Walking
// those trees is not free and an advert must stay quick, so a measurement
// is taken in the background and every advert carries the most recent one
// with the moment it was taken — a reader can then tell a fresh number
// from an hour-old one, and a partial walk from a complete one.
type Space struct {
	mu      sync.Mutex
	reading spaceReading
	where   string
	busy    bool
}

type spaceReading struct {
	workspace, state uint64
	at               time.Time
	partial          bool
}

// spaceBudget bounds a walk: a workspace full of dependency trees is
// measured approximately rather than holding the machine for a minute.
const (
	spaceBudget  = 8 * time.Second
	spaceEntries = 2_000_000
)

// Get is the last measurement, remeasured in the background when it is
// older than maxAge. It never blocks the caller: before the first walk
// finishes there is simply nothing to report yet.
func (s *Space) Get(workspaceRoot, stateDir string, maxAge time.Duration) spaceReading {
	s.mu.Lock()
	defer s.mu.Unlock()
	where := workspaceRoot + "\x00" + stateDir
	if where != s.where {
		s.reading, s.where = spaceReading{}, where
	}
	if !s.busy && time.Since(s.reading.at) > maxAge {
		s.busy = true
		go s.measure(workspaceRoot, stateDir)
	}
	return s.reading
}

func (s *Space) measure(workspaceRoot, stateDir string) {
	reading := spaceReading{at: time.Now().UTC()}
	deadline := time.Now().Add(spaceBudget)
	if workspaceRoot != "" {
		bytes, partial := DirectorySize(workspaceRoot, deadline)
		reading.workspace, reading.partial = bytes, reading.partial || partial
	}
	// A state directory kept inside the workspace is already counted
	// there; reporting it twice would overstate what Steve holds.
	if stateDir != "" && !within(stateDir, workspaceRoot) {
		bytes, partial := DirectorySize(stateDir, deadline)
		reading.state, reading.partial = bytes, reading.partial || partial
	}
	s.mu.Lock()
	s.reading, s.busy = reading, false
	s.mu.Unlock()
}

// within reports whether path sits inside root.
func within(path, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// DirectorySize adds up the files under dir without following symlinks,
// stopping at the deadline or the entry cap and saying so when it does.
func DirectorySize(dir string, deadline time.Time) (uint64, bool) {
	var total uint64
	var seen int
	partial := false
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable corner is skipped, not fatal: the number is
			// what this process can see of its own directories.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if seen++; seen > spaceEntries || (seen%2048 == 0 && time.Now().After(deadline)) {
			partial = true
			return filepath.SkipAll
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.Size() > 0 {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, partial
}
