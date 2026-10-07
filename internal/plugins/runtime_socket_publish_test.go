package plugins

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func socketPublicationFixture(t *testing.T) (*Store, RuntimeRecord, *os.Root, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket filesystem publication")
	}
	store, record := runtimeUsageFixture(t)
	root, err := os.OpenRoot(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	dir, err := os.MkdirTemp("/tmp", "steve-plugin-socket-publish-")
	if err != nil {
		t.Fatal(err)
	}
	cleanupSocketDir(t, filepath.Join(dir, "mcp.sock"))
	return store, record, root, dir
}
func TestRuntimeSocketWriteFailureReclaimsOnlyUnpublishedDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission refusal needs an unprivileged test process")
	}
	store, record, root, dir := socketPublicationFixture(t)
	parent := store.RuntimeDir(record.Ref.ID)
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0700) })
	location, err := store.publishRuntimeSocket(root, record.Ref.ID, dir)
	if location != "" || !os.IsPermission(err) {
		t.Fatalf("publication did not fail before rename: %q %v", location, err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "socket.json")); !os.IsNotExist(err) {
		t.Fatalf("unpublished record unexpectedly exists: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("certainly unpublished empty socket directory leaked: %v", err)
	}
}

func TestRuntimeSocketUnpublishedCleanupPreservesUnexpectedContents(t *testing.T) {
	store, record, root, dir := socketPublicationFixture(t)
	marker := filepath.Join(dir, "retained")
	if err := os.WriteFile(marker, []byte("preserve test-owned content"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(marker) })
	_, err := store.publishRuntimeSocket(root, "missing-"+record.Ref.ID, dir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication failure lost its original cause: %v", err)
	}
	data, readErr := os.ReadFile(marker)
	if readErr != nil || string(data) != "preserve test-owned content" {
		t.Fatalf("unpublished cleanup deleted unexpected contents: %q %v", data, readErr)
	}
}

func TestRuntimeSocketPostRenameFailureRetainsPotentiallyIssuedAddress(t *testing.T) {
	for _, mode := range []string{"published", "unreadable", "changed"} {
		t.Run(mode, func(t *testing.T) {
			store, record, root, dir := socketPublicationFixture(t)
			path := filepath.Join(store.RuntimeDir(record.Ref.ID), "socket.json")
			fault := errors.New("socket parent sync rejected")
			var issued string
			store.syncDir = func(parent string) error {
				if parent != store.RuntimeDir(record.Ref.ID) {
					return (&Store{}).sync(parent)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &issued); err != nil {
					t.Fatal(err)
				}
				if mode == "unreadable" {
					if err := os.Rename(path, path+".retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else if mode == "changed" {
					if err := os.WriteFile(path, []byte(`"other-address"`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return fault
			}
			location, err := store.publishRuntimeSocket(root, record.Ref.ID, dir)
			if location != "" || !errors.Is(err, fault) || issued != filepath.Join(dir, "mcp.sock") {
				t.Fatalf("failed to reach the real post-rename sync boundary: %q %v", location, err)
			}
			if info, statErr := os.Lstat(dir); statErr != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
				t.Fatalf("potentially issued socket directory was lost: %v %v", info, statErr)
			}
			if mode == "published" {
				store.syncDir = nil
				replayed, replayErr := store.RuntimeSocket(t.Context(), record.Ref)
				if replayErr != nil || replayed != issued {
					t.Fatalf("post-rename failure changed the saved address: %q %v", replayed, replayErr)
				}
			} else if !strings.Contains(err.Error(), "publication is uncertain") {
				t.Fatalf("uncertain publication was not reported: %v", err)
			}
		})
	}
}
