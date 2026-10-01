package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenCancellationWhileGitOwnsItsConfigLockDoesNotPoisonRetry(t *testing.T) {
	compiler, err := exec.LookPath("gcc")
	if errors.Is(err, exec.ErrNotFound) {
		t.Skip("gcc is needed for the child-only lock fixture")
	}
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	control, parent := t.TempDir(), t.TempDir()
	library := filepath.Join(control, "hold-config-lock.so")
	ctx, finish := context.WithTimeout(t.Context(), 20*time.Second)
	defer finish()
	build := exec.CommandContext(ctx, compiler, "-shared", "-fPIC", "-O2", "-o", library, "testdata/hold_config_lock.c", "-ldl")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build child lock fixture: %v: %s", err, out)
	}
	ready, gate := filepath.Join(control, "ready"), filepath.Join(control, "gate")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	wrapper := "#!/bin/sh\nif [ \"$1\" = init ]; then\n" +
		"  LD_PRELOAD=" + quote(library) + " GITREPO_LOCK_ROOT=" + quote(parent+string(os.PathSeparator)) +
		" GITREPO_LOCK_READY=" + quote(ready) + " GITREPO_LOCK_GATE=" + quote(gate) + " exec " + quote(realGit) + " \"$@\"\nfi\n" +
		"exec " + quote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(control, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", control+string(os.PathListSeparator)+os.Getenv("PATH"))
	first, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	dir := filepath.Join(parent, "p.git")
	go func() { _, err := Open(first, dir); result <- err }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-result:
			case <-time.After(5 * time.Second):
				t.Error("cancelled initializer did not exit")
			}
		}
	}()
	waitInitializationFile(t, ctx, ready)
	lock, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(lock)); err != nil {
		t.Fatalf("git did not actually acquire its config lock: %v", err)
	}
	cancel()
	select {
	case err := <-result:
		joined = true
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled initialization: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("initialization ignored cancellation")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Errorf("cancelled initialization polluted its publication directory: %v, %v", entries, err)
	}
	repo, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("initialization retry failed after the first git was cancelled: %v", err)
	}
	work := t.TempDir()
	write(t, work, "answer", "ready")
	if _, _, err := repo.Snapshot(ctx, work, "", "after cancelled initialization", false); err != nil {
		t.Fatalf("retry did not produce a usable repository: %v", err)
	}
}
