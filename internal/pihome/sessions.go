package pihome

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const classes = "classes.json"

// classRetention is how long a session's class is remembered after it was
// last opened. A session forgotten past it is still never resumed into the
// wrong tools: the launcher's --tools and its home check hold without it.
const classRetention = 180 * 24 * time.Hour

type classRecord struct {
	Class string    `json:"class"`
	At    time.Time `json:"at"`
}

type classFile struct {
	Version  int                    `json:"version"`
	Sessions map[string]classRecord `json:"sessions"`
}

// Remember records that a pi session was opened by a process started from
// the home of class.
func Remember(env []string, session, class string) error {
	base := lookup(env, EnvHomes)
	if base == "" || session == "" {
		return nil
	}
	unlock, err := lockBase(base)
	if err != nil {
		return err
	}
	defer unlock()
	known := readClasses(base)
	now := time.Now().UTC()
	for id, record := range known.Sessions {
		if now.Sub(record.At) > classRetention {
			delete(known.Sessions, id)
		}
	}
	known.Sessions[session] = classRecord{Class: class, At: now}
	data, err := json.MarshalIndent(known, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(base, classes), append(data, '\n'), 0o600)
}

// Resumable reports whether a pi session may be resumed by a process of
// class. One Steve recorded under the other class, or whose file pi-acp
// keeps outside this class's home, starts afresh instead. A session Steve
// has no record of is otherwise resumable; the launcher still refuses its
// file if it lives in the other home.
func Resumable(env []string, session, class string) bool {
	base := lookup(env, EnvHomes)
	if base == "" || session == "" {
		return true
	}
	unlock, err := lockBase(base)
	if err != nil {
		return false
	}
	recorded, ok := readClasses(base).Sessions[session]
	unlock()
	if ok && recorded.Class != class {
		return false
	}
	file := mappedSessionFile(env, session)
	return file == "" || inside(file, filepath.Join(Dir(base, class), "sessions"))
}

// Refused reports whether the launcher of class refused to resume session,
// and clears its note: the open that failed is to start afresh.
func Refused(env []string, session, class string) bool {
	base := lookup(env, EnvHomes)
	if base == "" || session == "" {
		return false
	}
	dir := filepath.Join(Dir(base, class), refused)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	found := false
	for _, entry := range entries {
		if noteSession(entry.Name()) == session {
			found = true
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
	return found
}

// noteSession is the session a refusal note names. The note is the refused
// file's base name, which pi gives as <time>_<session>.jsonl.
func noteSession(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	if i := strings.LastIndex(name, "_"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// readClasses is the class record, with base locked. A file that does not
// parse is moved aside, never overwritten, and reads as empty: the session
// map and the launcher still keep a resume in its own home.
func readClasses(base string) classFile {
	known := classFile{Version: 1, Sessions: map[string]classRecord{}}
	path := filepath.Join(base, classes)
	raw, err := os.ReadFile(path)
	if err != nil {
		return known
	}
	var parsed classFile
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Version != 1 || parsed.Sessions == nil {
		aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
		if renameErr := os.Rename(path, aside); renameErr != nil {
			slog.Error(fmt.Sprintf("pihome: unreadable pi session classes %s could not be moved aside: %v", path, renameErr))
		} else {
			slog.Warn(fmt.Sprintf("pihome: unreadable pi session classes moved to %s", aside))
		}
		return known
	}
	return parsed
}

// mappedSessionFile is the session file pi-acp keeps for session in its map
// under the adapter's home, or "".
func mappedSessionFile(env []string, session string) string {
	home := lookup(env, "HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".pi", "pi-acp", "session-map.json"))
	if err != nil {
		return ""
	}
	var parsed struct {
		Sessions map[string]struct {
			SessionFile string `json:"sessionFile"`
		} `json:"sessions"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	return parsed.Sessions[session].SessionFile
}

// inside reports whether path, with links in its directory resolved, lies
// under dir.
func inside(path, dir string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, parent)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
