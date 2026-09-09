package plugins

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPreparePinsBytesAndReplaysWithoutSource(t *testing.T) {
	source := fixtureDirectory(t)
	bundle, err := ReadDirectory(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: filepath.Join(t.TempDir(), "plugins")}
	origin := Source{Kind: "directory", Location: source}
	first, err := store.Prepare(t.Context(), "install-1", bundle.Digest, origin)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != Prepared || first.Digest != bundle.Digest {
		t.Fatalf("receipt: %+v", first)
	}
	if err := os.WriteFile(filepath.Join(source, "skills/review/SKILL.md"), []byte("different source"), 0600); err != nil {
		t.Fatal(err)
	}
	pinned, err := store.Read(bundle.Digest)
	if err != nil || !bytes.Equal(pinned.Data, bundle.Data) {
		t.Fatalf("installed content changed: %v", err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	restarted := &Store{Dir: store.Dir}
	replay, err := restarted.Prepare(t.Context(), "install-1", bundle.Digest, origin)
	if err != nil || replay != first {
		t.Fatalf("replay changed: %+v %v", replay, err)
	}
	receipts, err := restarted.List()
	if err != nil || len(receipts) != 1 || receipts[0] != first {
		t.Fatalf("list: %+v %v", receipts, err)
	}
}

func TestPrepareRefusesChangedPreviewAndReleaseIdentity(t *testing.T) {
	dir := fixtureDirectory(t)
	before, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: filepath.Join(t.TempDir(), "plugins")}
	source := Source{Kind: "directory", Location: dir}
	if _, err := store.Prepare(t.Context(), "first", before.Digest, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills/review/SKILL.md"), []byte("new content"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prepare(t.Context(), "second", before.Digest, source); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("stale preview: %v", err)
	}
	if _, err := store.Prepare(t.Context(), "second", after.Digest, source); !errors.Is(err, ErrConflict) {
		t.Fatalf("release collision: %v", err)
	}
	if _, err := store.Prepare(t.Context(), "first", after.Digest, source); !errors.Is(err, ErrConflict) {
		t.Fatalf("command collision: %v", err)
	}
	manifest := after.Manifest
	manifest.Version = "0.2.0"
	writeManifest(t, dir, manifest)
	next, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prepare(t.Context(), "upgrade", next.Digest, source); err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{before.Digest, next.Digest} {
		if _, err := store.Read(digest); err != nil {
			t.Fatalf("version lost: %v", err)
		}
	}
}

func TestPreparationResumesEachDurabilityBoundary(t *testing.T) {
	for _, boundary := range []string{"requests", "packages", "receipts"} {
		t.Run(boundary, func(t *testing.T) {
			dir := fixtureDirectory(t)
			bundle, err := ReadDirectory(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "plugins")
			store := &Store{Dir: root}
			source := Source{Kind: "directory", Location: dir}
			fault := errors.New("injected directory sync failure")
			store.syncDir = func(path string) error {
				if path == filepath.Join(root, boundary) {
					return fault
				}
				return (&Store{}).sync(path)
			}
			if _, err := store.Prepare(t.Context(), "retry", bundle.Digest, source); !errors.Is(err, fault) {
				t.Fatalf("fault not reached: %v", err)
			}
			// Version ownership survives even before an installation has a receipt.
			if boundary == "requests" {
				changed := bundle.Manifest
				changed.Description = "different release"
				writeManifest(t, dir, changed)
				replacement, err := ReadDirectory(t.Context(), dir)
				if err != nil {
					t.Fatal(err)
				}
				store.syncDir = nil
				if _, err := store.Prepare(t.Context(), "different", replacement.Digest, source); !errors.Is(err, ErrConflict) {
					t.Fatalf("lost reservation: %v", err)
				}
				writeManifest(t, dir, bundle.Manifest)
			} else {
				// Published content can finish its receipt without fetching source again.
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			}
			restarted := &Store{Dir: root}
			receipt, err := restarted.Prepare(t.Context(), "retry", bundle.Digest, source)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Digest != bundle.Digest || receipt.State != Prepared {
				t.Fatalf("receipt: %+v", receipt)
			}
			records, err := restarted.List()
			if err != nil || len(records) != 1 {
				t.Fatalf("partial/duplicate receipt: %+v %v", records, err)
			}
		})
	}
}

func TestConcurrentPreparationsCannotReplaceOneRelease(t *testing.T) {
	first := fixtureDirectory(t)
	second := fixtureDirectory(t)
	if err := os.WriteFile(filepath.Join(second, "skills/review/SKILL.md"), []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := ReadDirectory(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReadDirectory(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "plugins")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, item := range []struct {
		directory string
		bundle    Bundle
	}{{first, a}, {second, b}} {
		wg.Go(func() {
			store := &Store{Dir: root}
			_, err := store.Prepare(t.Context(), []string{"first", "second"}[i], item.bundle.Digest, Source{Kind: "directory", Location: item.directory})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	receipts, err := (&Store{Dir: root}).List()
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts: %+v %v", receipts, err)
	}
}

func TestStoreRejectsCorruptionAndConfinedPathEscape(t *testing.T) {
	for _, target := range []string{"bundle", "content", "link", "receipt"} {
		t.Run(target, func(t *testing.T) {
			dir := fixtureDirectory(t)
			bundle, err := ReadDirectory(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			store := &Store{Dir: filepath.Join(t.TempDir(), "plugins")}
			source := Source{Kind: "directory", Location: dir}
			if _, err := store.Prepare(t.Context(), "original", bundle.Digest, source); err != nil {
				t.Fatal(err)
			}
			packageDir := filepath.Join(store.Dir, "packages", bundle.Digest)
			switch target {
			case "bundle":
				if err := os.WriteFile(filepath.Join(packageDir, "bundle.tar"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "content":
				if err := os.WriteFile(filepath.Join(packageDir, "content", "skills/review/SKILL.md"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "link":
				if err := os.RemoveAll(filepath.Join(packageDir, "content")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir, filepath.Join(packageDir, "content")); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				if err := os.WriteFile(filepath.Join(store.Dir, "receipts", "original.json"), []byte(`{"schema":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Prepare(t.Context(), "original", bundle.Digest, source); err == nil {
				t.Fatal("corruption was accepted or silently replaced")
			}
		})
	}
}

func TestStaleStagingDoesNotLeakIntoPreparedPackage(t *testing.T) {
	dir := fixtureDirectory(t)
	bundle, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: filepath.Join(t.TempDir(), "plugins")}
	if err := store.ensure(); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(store.Dir, "packages", ".prepare-crashed", "content")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "survivor"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prepare(t.Context(), "fresh", bundle.Digest, Source{Kind: "directory", Location: dir}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(bundle.Digest)
	if err != nil || !bytes.Equal(got.Data, bundle.Data) {
		t.Fatalf("stale data leaked: %v", err)
	}
}

func TestCancelledPreparationDoesNotCreateStore(t *testing.T) {
	dir := fixtureDirectory(t)
	bundle := example(t, "github")
	root := filepath.Join(t.TempDir(), "absent")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (&Store{Dir: root}).Prepare(ctx, "cancelled", bundle.Digest, Source{Kind: "directory", Location: dir}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled operation created store: %v", err)
	}
}
