package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMarkerWriteFailureProcess(t *testing.T) {
	mode := os.Getenv("GITREPO_MARKER_WRITE_FAILURE")
	if mode == "" {
		return
	}
	dir := unmarkedRepository(t)
	config := read(t, dir, "config")
	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	limited := previous
	limited.Cur = 0
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatal(err)
	}
	var first error
	if mode == "recognize" {
		_, first = Open(t.Context(), dir)
	} else {
		first = markInitialized(t.Context(), dir)
	}
	_, markerErr := os.Stat(filepath.Join(dir, initializedMarker))
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(first, syscall.EFBIG) {
		t.Fatalf("expected a real file-size-limit write failure: %v", first)
	}
	if !os.IsNotExist(markerErr) {
		t.Fatalf("failed marker write left a published marker: %v", markerErr)
	}
	if read(t, dir, "config") != config {
		t.Fatal("failed marker write changed configuration")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.lock")); !os.IsNotExist(err) {
		t.Fatalf("failed marker write kept the recognition lock: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".steve-initialized-") {
			t.Fatalf("failed marker write left private preparation %s", entry.Name())
		}
	}
	if _, err := Open(t.Context(), dir); err != nil {
		t.Fatalf("retry after the write limit was lifted: %v", err)
	}
	if read(t, dir, initializedMarker) != "1\n" {
		t.Fatal("retry did not publish the complete marker")
	}
}

func TestMarkerWriteFailureDoesNotPublishOrPoisonRetry(t *testing.T) {
	for _, mode := range []string{"recognize", "private"} {
		t.Run(mode, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestMarkerWriteFailureProcess$", "-test.v")
			cmd.Env = append(os.Environ(), "GITREPO_MARKER_WRITE_FAILURE="+mode)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("marker write failure in %s: %v:\n%s", mode, err, out)
			}
		})
	}
}
