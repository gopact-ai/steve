package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPreparedCheckpointSurvivesLedgerFailureAndRestart(t *testing.T) {
	ctx := context.Background()
	records := &recordingRecords{err: errors.New("coordinator lost authority")}
	s := newTestStore(t, "node-a", records, nil, testPolicy{}, Limits{})
	snapshot := testSnapshot(t, s, 1)
	if _, err := s.Commit(ctx, snapshot, nil); err == nil {
		t.Fatal("ledger failure ignored")
	}
	if entries, err := os.ReadDir(filepath.Join(s.cfg.Dir, "manifests")); err != nil || len(entries) != 0 {
		t.Fatalf("prepared checkpoint exposed as committed: %v", err)
	}
	if _, err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	records.err = nil
	reopened, err := Open(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m, err := reopened.Commit(ctx, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Verify(ctx, m); err != nil {
		t.Fatalf("restart lost pinned content: %v", err)
	}
	loaded, ok, err := reopened.Load(ctx, m.ID)
	if err != nil || !ok || loaded.ID != m.ID {
		t.Fatalf("committed checkpoint not indexed: %+v, %v", loaded, err)
	}
}

func TestFetchUsesSurvivingReplicaAndTransfersOnlyChanges(t *testing.T) {
	ctx := context.Background()
	transport := &recordingTransport{nodes: map[string]*Store{}}
	records := &recordingRecords{}
	a := newTestStore(t, "node-a", records, transport, testPolicy{}, Limits{})
	b := newTestStore(t, "node-b", nil, nil, testPolicy{}, Limits{})
	transport.nodes["node-a"], transport.nodes["node-b"] = a, b
	snapshot := testSnapshot(t, a, 2)
	if _, err := a.Commit(ctx, snapshot, []string{"node-b"}); err != nil {
		t.Fatal(err)
	}
	before := transport.puts
	content := []byte("package changed\n")
	ref := Reference(content)
	if err := a.PutBlob(ctx, snapshot.Scope, ref, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	snapshot.Workspace.Files[0].Blob = ref
	snapshot.Cursors.OutputPublished++
	m, err := a.Commit(ctx, snapshot, []string{"node-b"})
	if err != nil {
		t.Fatal(err)
	}
	if transport.puts != before+1 {
		t.Fatalf("unchanged context retransmitted: puts %d → %d", before, transport.puts)
	}
	// A redundant unavailable replica cannot prevent a checkpoint that still
	// reaches the explicitly required independent copies from committing.
	if _, err := a.Commit(ctx, snapshot, []string{"node-b", "node-offline"}); err != nil {
		t.Fatalf("optional unavailable replica blocked complete checkpoint: %v", err)
	}
	delete(transport.nodes, "node-a")
	c := newTestStore(t, "node-c", nil, transport, testPolicy{}, Limits{})
	verified, err := c.Fetch(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if verified.NodeID() != "node-c" {
		t.Fatal("fetched data not verified on destination")
	}
	var got bytes.Buffer
	if err := c.ReadBlob(ctx, snapshot.Scope, ref, &got); err != nil || !bytes.Equal(got.Bytes(), content) {
		t.Fatalf("surviving replica failed recovery: %v", err)
	}
}

type brokenReader struct{ sent bool }

func (r *brokenReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "partial"), nil
}

func TestPartialUploadHasNoReceiptAndCanRetry(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "node-a", nil, nil, testPolicy{}, Limits{})
	scope := Scope{ProjectID: "project-1", HomeNodeID: "node-a", Level: "internal"}
	data := []byte("partial then completed")
	ref := Reference(data)
	if err := s.PutBlob(ctx, scope, ref, &brokenReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial upload: %v", err)
	}
	if has, err := s.HasBlob(ctx, scope, ref); err != nil || has {
		t.Fatalf("partial bytes published: %v", err)
	}
	if err := s.PutBlob(ctx, scope, ref, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, scope, ref, bytes.NewReader(data)); err != nil {
		t.Fatalf("idempotent retry refused: %v", err)
	}
	if err := s.PutBlob(ctx, scope, ref, bytes.NewReader([]byte("wrong"))); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("repeated upload bypassed hash verification: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(s.cfg.Dir, "tmp")); err != nil || len(entries) != 0 {
		t.Fatalf("partial transfer temp files leaked: %v", err)
	}
}

func TestQuotaAndContentScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "node-a", nil, nil, testPolicy{}, Limits{MaxBytes: 9, MaxObjects: 2})
	scope := Scope{ProjectID: "project-1", Level: "sealed", HomeNodeID: "node-a"}
	data := []byte("123456789")
	if err := s.PutBlob(ctx, scope, Reference(data), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, scope, Reference([]byte("more")), strings.NewReader("more")); !errors.Is(err, ErrQuota) {
		t.Fatalf("whole-store quota ignored: %v", err)
	}
	other := scope
	other.Level = "public"
	if has, err := s.HasBlob(ctx, other, Reference(data)); err != nil || has {
		t.Fatalf("content bypassed classification through same digest: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.PutBlob(ctx, scope, Reference([]byte("more")), strings.NewReader("more")); !errors.Is(err, ErrQuota) {
		t.Fatalf("restart reset quota: %v", err)
	}
}

func TestCorruptContentAndUnsafePathsCannotRestore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "node-a", &recordingRecords{}, nil, testPolicy{}, Limits{})
	snapshot := testSnapshot(t, s, 1)
	for _, paths := range [][]string{{"../escape"}, {"/absolute"}, {"x\\escape"}, {"C:drive"}, {".ssh/../key"}, {"NUL"}, {"src/main.go", "SRC/Main.go"}, {"A/b", "a"}} {
		bad := snapshot
		bad.Workspace.Files = nil
		for _, name := range paths {
			bad.Workspace.Files = append(bad.Workspace.Files, File{Path: name, Blob: snapshot.Context})
		}
		if _, err := s.Commit(ctx, bad, nil); err == nil {
			t.Fatalf("unsafe path set accepted: %q", paths)
		}
	}
	m, err := s.Commit(ctx, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if _, err := s.Restore(ctx, m, directory); err == nil {
		t.Fatal("existing workspace replaced")
	}
	ref := snapshot.Workspace.Files[0].Blob
	if err := os.WriteFile(filepath.Join(s.cfg.Dir, blobName(snapshot.Scope, ref)), bytes.Repeat([]byte("x"), int(ref.Size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, m); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt local content accepted: %v", err)
	}
	if _, err := s.Restore(ctx, m, filepath.Join(t.TempDir(), "restore")); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt content restored: %v", err)
	}
}

func TestExclusiveStoreLockAndInterruptedPinGC(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "node-a", &recordingRecords{}, nil, testPolicy{}, Limits{})
	if second, err := Open(s.cfg); err == nil {
		_ = second.Close()
		t.Fatal("two writers share a store")
	}
	snapshot := testSnapshot(t, s, 1)
	if _, err := s.Prepare(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasBlob(ctx, snapshot.Scope, snapshot.Context); err != nil || !has {
		t.Fatalf("prepared content lost: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.Dir, "prepared", strings.Repeat("0", 64)+".json"), []byte(`{"snapshot":`), 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := []byte("still keep until index fixed")
	if err := s.PutBlob(ctx, snapshot.Scope, Reference(orphan), bytes.NewReader(orphan)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("GC ignored corrupt reachability index: %v", err)
	}
	if has, err := s.HasBlob(ctx, snapshot.Scope, Reference(orphan)); err != nil || !has {
		t.Fatalf("GC deleted data before checking all manifests: %v", err)
	}
}

func TestConcurrentUploadsRespectQuotaAndCancellation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "node-a", nil, nil, testPolicy{}, Limits{MaxBytes: 8})
	scope := Scope{ProjectID: "project-1", HomeNodeID: "node-a", Level: "internal"}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, text := range []string{"12345678", "abcdefgh"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.PutBlob(ctx, scope, Reference([]byte(text)), strings.NewReader(text))
		}()
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent quota admitted %d uploads", success)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.PutBlob(cancelled, scope, Reference([]byte("")), strings.NewReader("")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request wrote content: %v", err)
	}
	deadline, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if _, err := s.GC(deadline); err != nil {
		t.Fatal(err)
	}
}
