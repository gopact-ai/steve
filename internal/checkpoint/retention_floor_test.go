package checkpoint

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func floorFixture() (RetainedBlob, RetainedBlob, []byte, []byte) {
	scope := Scope{ProjectID: "p", Level: "internal", HomeNodeID: "node-a"}
	data, unknownData := []byte("explicitly released bytes"), []byte("unknown promise")
	r := RetainedBlob{ID: Reference([]byte("known")).SHA256, Sequence: 2, Scope: scope, Blob: Reference(data)}
	unknown := RetainedBlob{ID: Reference([]byte("unknown")).SHA256, Sequence: 1, Scope: scope, Blob: Reference(unknownData)}
	return r, unknown, data, unknownData
}

func TestRetainedFloorCrashRecovery(t *testing.T) {
	if dir := os.Getenv("STEVE_TEST_RETENTION_CRASH_DIR"); dir != "" {
		crashRetentionAt(t, dir, os.Getenv("STEVE_TEST_RETENTION_CRASH_CUT"))
		os.Exit(23) // Deliberately skip Close and all defers in this isolated child.
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []string{"floor", "rename", "unlink", "compact"} {
		t.Run(cut, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestRetainedFloorCrashRecovery$")
			cmd.Env = append(os.Environ(), "STEVE_TEST_RETENTION_CRASH_DIR="+dir, "STEVE_TEST_RETENTION_CRASH_CUT="+cut)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("child did not reach crash boundary: %v\n%s", err, output)
			}
			s, err := OpenRetained(Config{Dir: dir, NodeID: "node-a", Policy: testPolicy{}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, unknown, data, unknownData := floorFixture()
			var before retentionRecord
			if ok, err := s.readJSONLocked("retained/"+unknown.ID+".json", &before); err != nil || !ok {
				t.Fatalf("unknown lost on crash: %v", err)
			}
			result, err := s.CollectRetained(t.Context(), []RetainedBlob{r}, []RetainedBlob{unknown})
			if err != nil || len(result.Released) != 1 || result.Released[0] != r.ID {
				t.Fatalf("release retry after %s: %+v %v", cut, result, err)
			}
			if _, err := s.PutRetainedBlob(t.Context(), r, bytes.NewReader(data)); !errors.Is(err, ErrRetired) {
				t.Fatalf("old promise replayed after compaction: %v", err)
			}
			at, err := s.PutRetainedBlob(t.Context(), unknown, bytes.NewReader(unknownData))
			if err != nil || !at.Equal(before.StoredAt) {
				t.Fatalf("floor changed surviving unknown receipt: %v", err)
			}
			if s.objects != 3 {
				t.Fatalf("completed tombstone retained quota after %s: %d", cut, s.objects)
			}
			if result, err := s.CollectRetained(t.Context(), []RetainedBlob{r}, nil); err != nil || result.Blobs != 0 || len(result.Released) != 1 {
				t.Fatalf("lost acknowledgment was not repeatable: %+v %v", result, err)
			}
		})
	}
}

func crashRetentionAt(t *testing.T, dir, cut string) {
	t.Helper()
	s, err := OpenRetained(Config{Dir: dir, NodeID: "node-a", Policy: testPolicy{}})
	if err != nil {
		t.Fatal(err)
	}
	r, unknown, data, unknownData := floorFixture()
	for _, item := range []struct {
		retention RetainedBlob
		data      []byte
	}{{r, data}, {unknown, unknownData}} {
		if _, err := s.PutRetainedBlob(t.Context(), item.retention, bytes.NewReader(item.data)); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	if err := s.advanceRetentionFloorLocked(r.Sequence); err != nil {
		t.Fatal(err)
	}
	if cut == "floor" {
		return
	}
	active, retired, err := s.retentionsLocked(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	candidates := map[string]RetainedBlob{r.ID: r}
	if err := s.retireMarkersLocked(t.Context(), candidates, active, retired); err != nil {
		t.Fatal(err)
	}
	if cut == "rename" {
		return
	}
	if _, err := s.gcLocked(t.Context(), map[string]bool{blobName(r.Scope, r.Blob): true}); err != nil {
		t.Fatal(err)
	}
	if cut == "unlink" {
		return
	}
	if _, err := s.compactRetiredLocked(t.Context(), candidates); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedFloorLossIsNotTreatedAsFreshAdmission(t *testing.T) {
	for _, damage := range []string{"missing", "checksum"} {
		t.Run(damage, func(t *testing.T) {
			cfg := Config{Dir: t.TempDir(), NodeID: "node-a", Policy: testPolicy{}}
			s, err := OpenRetained(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r, _, data, _ := floorFixture()
			if _, err := s.PutRetainedBlob(t.Context(), r, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CollectRetained(t.Context(), []RetainedBlob{r}, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(cfg.Dir, retentionFloorName)
			if damage == "missing" {
				err = os.Remove(name)
			} else {
				raw := []byte(retentionFloorData(r.Sequence))
				raw[15] = '1' // Structurally valid but changed without its checksum.
				err = os.WriteFile(name, raw, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if reopened, err := OpenRetained(cfg); !errors.Is(err, ErrIntegrity) {
				if reopened != nil {
					_ = reopened.Close()
				}
				t.Fatalf("damaged floor reopened admission: %v", err)
			}
		})
	}
}

func TestRequiredReceiptCannotAlsoBeACollectionCandidate(t *testing.T) {
	s := newTestStore(t, "node-a", nil, nil, testPolicy{}, Limits{})
	r, _, data, _ := floorFixture()
	if _, err := s.PutRetainedBlob(t.Context(), r, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CollectRetained(t.Context(), []RetainedBlob{r}, []RetainedBlob{r}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("contradictory release bypassed live root: %v", err)
	}
	if _, err := s.PutRetainedBlob(t.Context(), r, bytes.NewReader(data)); err != nil {
		t.Fatalf("rejected release changed live admission: %v", err)
	}
}
