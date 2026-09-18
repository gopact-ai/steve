package node

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func settled(t *testing.T, space *Space, workspace, state string) spaceReading {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if reading := space.Get(workspace, state, time.Minute); !reading.at.IsZero() {
			return reading
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no measurement arrived")
	return spaceReading{}
}

func TestSpaceReportsWhatSteveHoldsWithoutCountingNestedStateTwice(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "work")
	writeSized(t, filepath.Join(workspace, "projects", "one", "file.bin"), 4096)
	writeSized(t, filepath.Join(workspace, "state", "ledger.db"), 2048)
	if err := os.Symlink(filepath.Join(workspace, "projects", "one", "file.bin"), filepath.Join(workspace, "link.bin")); err != nil {
		t.Fatal(err)
	}
	var space Space
	nested := settled(t, &space, workspace, filepath.Join(workspace, "state"))
	if nested.workspace != 6144 {
		t.Fatalf("workspace bytes = %d, want 6144 (symlinks are not followed)", nested.workspace)
	}
	if nested.state != 0 {
		t.Fatalf("state bytes = %d, want 0: a state directory inside the workspace is already counted there", nested.state)
	}
	if nested.partial {
		t.Fatal("a small tree must be measured completely")
	}

	outside := filepath.Join(root, "state")
	writeSized(t, filepath.Join(outside, "ledger.db"), 1024)
	var separate Space
	apart := settled(t, &separate, workspace, outside)
	if apart.workspace != 6144 || apart.state != 1024 {
		t.Fatalf("workspace=%d state=%d, want 6144 and 1024", apart.workspace, apart.state)
	}
}

func TestSpaceKeepsOneReadingPerDirectoryPair(t *testing.T) {
	// A hub and the node on the same machine both ask this process about
	// their own directories. A single shared slot let each wipe the
	// other's answer, so neither ever reported anything.
	first, second := t.TempDir(), t.TempDir()
	writeSized(t, filepath.Join(first, "a.bin"), 1024)
	writeSized(t, filepath.Join(second, "b.bin"), 2048)
	var space Space
	settled(t, &space, first, "")
	settled(t, &space, second, "")
	for _, want := range []struct {
		dir   string
		bytes uint64
	}{{first, 1024}, {second, 2048}} {
		reading := space.Get(want.dir, "", time.Minute)
		if reading.workspace != want.bytes || reading.at.IsZero() {
			t.Fatalf("%s reports %d at %v, want %d and a measurement time", want.dir, reading.workspace, reading.at, want.bytes)
		}
	}
}

func TestSpaceServesTheLastWalkAndRemeasuresWhenTheDirectoryChanges(t *testing.T) {
	workspace := t.TempDir()
	writeSized(t, filepath.Join(workspace, "file.bin"), 1024)
	var space Space
	first := settled(t, &space, workspace, "")
	writeSized(t, filepath.Join(workspace, "more.bin"), 1024)
	// A fresh reading is served as it stands: the point of measuring in
	// the background is that an advert never waits for a walk.
	if again := space.Get(workspace, "", time.Minute); again.workspace != first.workspace || again.at != first.at {
		t.Fatalf("cached reading changed without a remeasure: %d at %v", again.workspace, again.at)
	}
	space.Get(workspace, "", 0)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if reading := space.Get(workspace, "", time.Minute); reading.workspace == 2048 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a stale reading was never remeasured")
}

func TestHealthCarriesTheMeasuredRootAndSpace(t *testing.T) {
	workspace := t.TempDir()
	writeSized(t, filepath.Join(workspace, "wt-one", "file.bin"), 512)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		health := CheckHealth(workspace, "")
		if health.Root != workspace {
			t.Fatalf("health root = %q, want %q", health.Root, workspace)
		}
		if health.Worktrees != 1 {
			t.Fatalf("worktrees = %d, want 1", health.Worktrees)
		}
		if health.WorkspaceBytes == 512 && !health.SpaceAt.IsZero() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("health never carried the measured space")
}
