package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInitializationMarkerRequiresItsCompleteFormat(t *testing.T) {
	for _, contents := range []string{"", "1", "2\n", "1\nextra", strings.Repeat("x", 4096)} {
		t.Run("length-"+strconv.Itoa(len(contents)), func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, initializedMarker, contents)
			if ready, err := initialized(dir); ready || err == nil {
				t.Fatalf("accepted incomplete or unknown marker %q: ready=%v err=%v", contents[:min(len(contents), 16)], ready, err)
			}
			if _, err := Open(t.Context(), dir); err == nil {
				t.Fatal("Open trusted a marker that never completed")
			}
			if read(t, dir, initializedMarker) != contents {
				t.Fatal("changed an unknown marker")
			}
		})
	}
}

func TestMarkerPublicationDoesNotReplaceAnExistingTarget(t *testing.T) {
	for _, kind := range []string{"valid", "empty", "unknown", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, initializedMarker)
			contents := "1\n"
			switch kind {
			case "empty":
				contents = ""
			case "unknown":
				contents = "unrecognized\n"
			}
			switch kind {
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "marker")
				if err := os.WriteFile(target, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			default:
				write(t, dir, initializedMarker, contents)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			err = writeInitializationMarker(t.Context(), dir)
			if kind == "valid" && err != nil || kind != "valid" && err == nil {
				t.Errorf("publication over %s: %v", kind, err)
			}
			after, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(before, after) {
				t.Fatalf("replaced an existing %s marker: %v", kind, statErr)
			}
			if kind != "directory" && read(t, dir, initializedMarker) != contents {
				t.Fatal("overwrote an existing marker")
			}
		})
	}
}

type markerPreparedCancellation struct {
	context.Context
	dir      string
	cancel   context.CancelFunc
	observed bool
}

func (c *markerPreparedCancellation) Err() error {
	entries, _ := os.ReadDir(c.dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".steve-initialized-") {
			contents, err := os.ReadFile(filepath.Join(c.dir, entry.Name()))
			if err == nil && string(contents) == "1\n" {
				c.observed = true
				c.cancel()
				return context.Canceled
			}
		}
	}
	return c.Context.Err()
}

func TestMarkerPublicationChecksCancellationAfterPreparation(t *testing.T) {
	dir := t.TempDir()
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &markerPreparedCancellation{Context: base, dir: dir, cancel: cancel}
	if err := writeInitializationMarker(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("marker published after preparation cancelled its context: %v", err)
	}
	if !ctx.observed {
		t.Fatal("cancellation did not observe a complete private marker")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled preparation left a marker or staging file: %v, %v", entries, err)
	}
}

// Close is the preparation boundary: publishing before a successful close
// would expose data whose completed write was never acknowledged.
type markerCloseResult struct {
	*os.File
	failure error
	after   func()
}

func (f markerCloseResult) Close() error {
	err := f.File.Close()
	if f.after != nil {
		f.after()
	}
	return errors.Join(err, f.failure)
}

func TestMarkerPublicationRequiresSuccessfulClose(t *testing.T) {
	dir := t.TempDir()
	file, err := os.CreateTemp(dir, ".steve-initialized-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	failure := errors.New("marker close failed")
	err = publishInitializationMarker(t.Context(), dir, file.Name(), markerCloseResult{File: file, failure: failure})
	if !errors.Is(err, failure) {
		t.Fatalf("close failure was lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, initializedMarker)); !os.IsNotExist(err) {
		t.Fatalf("close failure published a ready marker: %v", err)
	}
	contents, err := os.ReadFile(file.Name())
	if err != nil || string(contents) != initializedContents {
		t.Fatalf("close injection was not after the complete write: %q, %v", contents, err)
	}
}

func TestMarkerPublicationPreservesATargetArrivingAtClose(t *testing.T) {
	for _, contents := range []string{"1\n", "", "another owner\n"} {
		t.Run("length-"+strconv.Itoa(len(contents)), func(t *testing.T) {
			dir := t.TempDir()
			file, err := os.CreateTemp(dir, ".steve-initialized-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(file.Name())
			path := filepath.Join(dir, initializedMarker)
			var before os.FileInfo
			afterClose := func() {
				write(t, dir, initializedMarker, contents)
				before, err = os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			err = publishInitializationMarker(t.Context(), dir, file.Name(), markerCloseResult{File: file, after: afterClose})
			if contents == initializedContents && err != nil || contents != initializedContents && err == nil {
				t.Errorf("publication over arriving contents %q: %v", contents, err)
			}
			after, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(before, after) || read(t, dir, initializedMarker) != contents {
				t.Fatalf("publication replaced an arriving marker: %v", statErr)
			}
		})
	}
}

func TestCompletedMarkerSurvivesLaterCancellation(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := writeInitializationMarker(ctx, dir); err != nil {
		t.Fatal(err)
	}
	cancel()
	if ready, err := initialized(dir); err != nil || !ready {
		t.Fatalf("published marker lost after cancellation: ready=%v err=%v", ready, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != initializedMarker {
		t.Fatalf("publication left private marker files: %v, %v", entries, err)
	}
}
