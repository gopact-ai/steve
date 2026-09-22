//go:build unix

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestAcquireLockRejectsLinkedSentinelWithoutChangingIt(t *testing.T) {
	for _, kind := range []string{"hard-link", "symbolic-link"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "state")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			// The target is outside the lock directory, but on the same
			// filesystem so this exercises a real hard link, not a mock.
			sentinel := filepath.Join(root, "sentinel")
			const contents = "unrelated private data must survive lock acquisition\n"
			if err := os.WriteFile(sentinel, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "gateway.lock")
			link := os.Link
			if kind == "symbolic-link" {
				link = os.Symlink
			}
			if err := link(sentinel, path); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			release, err := AcquireLock(dir)
			if release != nil {
				release()
			}
			if err == nil {
				t.Error("accepted a linked lock file")
			}
			if err != nil && release != nil {
				t.Error("refused lock returned a release function")
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != contents {
				t.Errorf("lock acquisition changed the outside sentinel: %q, %v", data, err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Errorf("refusal replaced or changed the lock link: %v", err)
			}
		})
	}
}

func TestAcquireLockRejectsDanglingSymlinkWithoutCreatingTarget(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "absent-sentinel")
	path := filepath.Join(dir, "gateway.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireLock(dir)
	if release != nil {
		release()
	}
	if err == nil {
		t.Error("accepted a dangling lock symlink")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("lock acquisition created the outside target: %v", err)
	}
}

func TestAcquireLockRequiresPrivateRegularFile(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0700, 0640, 0604, 0620, 0602, 0644, 0666} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "gateway.lock")
			const contents = "previous lock owner\n"
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			release, err := AcquireLock(dir)
			if release != nil {
				release()
			}
			if mode&0077 != 0 {
				if err == nil {
					t.Error("accepted a non-private lock file")
				} else if strings.Contains(err.Error(), "another gateway") {
					t.Errorf("unsafe file was reported as lock contention: %v", err)
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != contents {
					t.Errorf("refusal changed lock contents: %q, %v", data, err)
				}
			} else if err != nil {
				t.Fatalf("private regular lock refused: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("lock acquisition changed permissions: %v", err)
			}
		})
	}
	t.Run("fifo", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "gateway.lock")
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		release, err := AcquireLock(dir)
		if release != nil {
			release()
		}
		if err == nil {
			t.Fatal("accepted a FIFO as the gateway lock")
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("refusal replaced the FIFO: %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "gateway.lock"), 0700); err != nil {
			t.Fatal(err)
		}
		release, err := AcquireLock(dir)
		if release != nil {
			release()
		}
		if err == nil {
			t.Fatal("accepted a directory as the gateway lock")
		}
	})
}

func TestAcquireLockRefusesASecondGateway(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	path := filepath.Join(dir, "gateway.lock")
	want := fmt.Sprintf("%d\n", os.Getpid())
	if data, err := os.ReadFile(path); err != nil || string(data) != want {
		t.Fatalf("lock owner hint = %q, %v", data, err)
	}
	// A contender in this process would write the same PID. Use distinct
	// contents to detect truncation before a failed flock.
	want = "held lock; preserve the owner's diagnostic contents\n"
	if err := os.WriteFile(path, []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(dir); err == nil || !strings.Contains(err.Error(), "another gateway") {
		t.Fatalf("second acquire = %v, want a refusal", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != want {
		t.Fatalf("failed contender changed owner hint = %q, %v", data, err)
	}
	release()
	release = nil
	again, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again()
}
