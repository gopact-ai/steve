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

// Pause at a configuration read, not by delaying Git initialization. The
// old per-key reader has already read user.name by the gc.auto boundary;
// the snapshot reader has acquired its writer exclusion before reading.
func holdRecognitionRead(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	control := t.TempDir()
	ready, gate := filepath.Join(control, "ready"), filepath.Join(control, "gate")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := os.OpenFile(gate, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release.Close() })
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	wrapper := "#!/bin/sh\ncase \"$*\" in\n" +
		" 'config --local --get gc.auto'|'config --local --null --list')\n" +
		"  printf ready > " + quote(ready) + "\n" +
		"  read release < " + quote(gate) + "\n;;\nesac\n" +
		"exec " + quote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(control, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", control+string(os.PathListSeparator)+os.Getenv("PATH"))
	return realGit, func() {
		waitInitializationFile(t, ctx, ready)
		if _, err := release.WriteString("continue\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func beginRecognition(t *testing.T, ctx context.Context, dir string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := Open(ctx, dir)
		done <- err
		close(done)
	}()
	return done
}

func recognitionReadyPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0], "ready")
}

func unmarkedRepository(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "p.git")
	if _, err := Open(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, initializedMarker)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRecognitionExcludesConfigChangesUntilItMarksTheRepository(t *testing.T) {
	for _, valid := range []bool{false, true} {
		name := "invalid configuration cannot be pieced together"
		if valid {
			name = "valid configuration stays valid through marking"
		}
		t.Run(name, func(t *testing.T) {
			dir := unmarkedRepository(t)
			if !valid {
				if _, err := (&Repo{Dir: dir}).Git(t.Context(), nil, "config", "gc.auto", "1"); err != nil {
					t.Fatal(err)
				}
			}
			before := read(t, dir, "config")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			realGit, release := holdRecognitionRead(t, ctx)
			done := beginRecognition(t, ctx, dir)
			defer func() { cancel(); <-done }()
			waitInitializationFile(t, ctx, recognitionReadyPath(t))
			// These are actual Git writes and use its normal config.lock.
			for _, setting := range [][2]string{{"user.name", "other-owner"}, {"gc.auto", "0"}} {
				cmd := exec.CommandContext(ctx, realGit, "--git-dir="+dir, "config", setting[0], setting[1])
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(out), "config.lock") {
					t.Errorf("configuration writer was not excluded at %s: %v: %s", setting[0], err, out)
				}
			}
			release()
			err := <-done
			if valid && err != nil || !valid && err == nil {
				t.Errorf("recognizing valid=%v: %v", valid, err)
			}
			if read(t, dir, "config") != before {
				t.Error("configuration changed while it was being recognized")
			}
			if _, err := os.Stat(filepath.Join(dir, "config.lock")); !os.IsNotExist(err) {
				t.Errorf("recognition left its configuration lock: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, initializedMarker)); valid && err != nil || !valid && !os.IsNotExist(err) {
				t.Errorf("marker for valid=%v: %v", valid, err)
			}
		})
	}
}

func TestRecognitionReleasesOnlyItsLockOnCancellationOrMarkerFailure(t *testing.T) {
	for _, failure := range []string{"cancel", "marker-directory", "replaced-lock"} {
		t.Run(failure, func(t *testing.T) {
			dir := unmarkedRepository(t)
			before := read(t, dir, "config")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, release := holdRecognitionRead(t, ctx)
			done := beginRecognition(t, ctx, dir)
			defer func() { cancel(); <-done }()
			waitInitializationFile(t, ctx, recognitionReadyPath(t))
			lock := filepath.Join(dir, "config.lock")
			if _, err := os.Stat(lock); err != nil {
				t.Errorf("recognition has no exclusive configuration lock: %v", err)
			}
			switch failure {
			case "cancel":
				cancel()
			case "marker-directory":
				if err := os.Mkdir(filepath.Join(dir, initializedMarker), 0700); err != nil {
					t.Fatal(err)
				}
				release()
			case "replaced-lock":
				if err := os.Remove(lock); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				write(t, dir, "config.lock", "a different owner\n")
				release()
			}
			err := <-done
			if err == nil || failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Errorf("recognition after %s: %v", failure, err)
			}
			if read(t, dir, "config") != before {
				t.Error("failed recognition changed configuration")
			}
			if failure == "replaced-lock" {
				if read(t, dir, "config.lock") != "a different owner\n" {
					t.Error("recognition removed or changed a replacement lock")
				}
			} else if _, err := os.Stat(lock); !os.IsNotExist(err) {
				t.Errorf("failed recognition left its own lock: %v", err)
			}
			if failure != "marker-directory" {
				if _, err := os.Stat(filepath.Join(dir, initializedMarker)); !os.IsNotExist(err) {
					t.Errorf("failed recognition still marked the repository: %v", err)
				}
			}
		})
	}
}
