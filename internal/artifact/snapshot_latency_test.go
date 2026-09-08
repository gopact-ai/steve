package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Snapshot is on both sides of every ordinary turn. Keep its unchanged
// path from growing a separate index scan for each preflight check.
func TestUnchangedSnapshotGitProcessBudget(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	parent, _, err := repo.Snapshot(t.Context(), work, "", "initial", false)
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "git-trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	sha, changed, err := repo.Snapshot(t.Context(), work, parent, "unchanged", false)
	if err != nil || changed || sha != parent {
		t.Fatalf("unchanged snapshot: sha=%q changed=%v err=%v", sha, changed, err)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "start" {
			commands = append(commands, event.Argv)
		}
	}
	if len(commands) == 0 || len(commands) > 6 {
		t.Fatalf("unchanged snapshot used %d Git processes; budget 6: %v", len(commands), commands)
	}
}

func TestSnapshotInheritedGitlinkLimitsAndLiteralNames(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := repo.git(t.Context(), env, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",nested"); err != nil {
		t.Fatal(err)
	}
	tree, err := repo.git(t.Context(), env, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := repo.git(t.Context(), nil, "commit-tree", strings.TrimSpace(tree), "-m", "gitlink")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "nested/large", strings.Repeat("x", 64))
	// These are literal untracked names, not staged-entry metadata.
	for _, name := range []string{"160000 " + strings.Repeat("a", 40) + " 0\tpretend", "H 160000\tpretend", "? spaced\nname"} {
		write(t, work, name, "ok")
	}
	repo.Limits.MaxFileBytes = 32
	if sha, changed, err := repo.Snapshot(t.Context(), work, strings.TrimSpace(parent), "too large", false); err == nil || changed || sha != "" {
		t.Fatalf("gitlink hid the oversized replacement: %q %v %v", sha, changed, err)
	} else {
		var limit TooLarge
		if !errors.As(err, &limit) || limit != (TooLarge{"file_bytes", 64, 32}) {
			t.Fatalf("unexpected limit error: %v", err)
		}
	}
	repo.Limits.MaxFileBytes = 64
	sha, changed, err := repo.Snapshot(t.Context(), work, strings.TrimSpace(parent), "replacement", false)
	if err != nil || !changed {
		t.Fatalf("replacement: %q %v %v", sha, changed, err)
	}
	paths, err := repo.Changed(t.Context(), "", sha)
	if err != nil || len(paths) != 4 {
		t.Fatalf("replacement tree lost literal files: %v %v", paths, err)
	}
	if again, changed, err := repo.Snapshot(t.Context(), work, sha, "unchanged", false); err != nil || changed || again != sha {
		t.Fatalf("tracked literal names changed: %q %v %v", again, changed, err)
	}
}

func BenchmarkSnapshotUnchanged(b *testing.B) {
	for _, count := range []int{0, 100} {
		b.Run(fmt.Sprintf("files=%d", count), func(b *testing.B) {
			repo, err := Open(b.Context(), filepath.Join(b.TempDir(), "objects.git"))
			if err != nil {
				b.Fatal(err)
			}
			work := b.TempDir()
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file-%03d", i)), []byte("contents\n"), 0o644); err != nil {
					b.Fatal(err)
				}
			}
			parent, _, err := repo.Snapshot(b.Context(), work, "", "initial", false)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sha, changed, err := repo.Snapshot(b.Context(), work, parent, "unchanged", false)
				if err != nil || changed || sha != parent {
					b.Fatalf("unchanged snapshot: %q %v %v", sha, changed, err)
				}
			}
		})
	}
}
