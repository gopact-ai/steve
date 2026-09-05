package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A machine's own skills are the ones its AI tools were given outside
// Steve: directories with a SKILL.md under the tools' home directories.
// Every machine scans its own and reports them in its advert; the hub
// keeps the last advert, so a page reads what a machine last said, and
// asking again is a refresh, not a wait.

// localDirs are where the AI tools keep skills of their own, under the
// user's home. Steve's isolated runtime homes are not among them: what
// is there came from the hub.
var localDirs = []string{".codex/skills", ".claude/skills", ".grok/skills", ".kimi/skills", ".agents/skills"}

// ScanLocal lists the skills under home's tool directories, each once by
// its physical path — a tool that links its directory to another's
// (~/.agents/skills is often ~/.codex/skills) would otherwise list every
// skill twice.
func ScanLocal(home string) []Found {
	if home == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []Found
	for _, rel := range localDirs {
		dir := filepath.Join(home, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if !hasSkill(path) {
				continue
			}
			real, err := filepath.EvalSymlinks(path)
			if err != nil {
				continue
			}
			if seen[real] {
				continue
			}
			seen[real] = true
			d := Describe(real)
			out = append(out, Found{Name: e.Name(), Path: real, Title: d.Title, Description: d.Description})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Local is a cached scan of this machine's own skills: what the advert
// carries, redone when older than the time given, or when asked.
type Local struct {
	mu   sync.Mutex
	at   time.Time
	list []Found
}

// Get is the last scan, redone when older than maxAge.
func (l *Local) Get(maxAge time.Duration) []Found {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.at) > maxAge {
		home, _ := os.UserHomeDir()
		l.list, l.at = ScanLocal(home), time.Now()
	}
	return append([]Found{}, l.list...)
}

// Rescan throws the cache away.
func (l *Local) Rescan() []Found { return l.Get(0) }
