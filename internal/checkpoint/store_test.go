package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type recordingRecords struct {
	items []Manifest
	err   error
}

func (r *recordingRecords) RecordCheckpoint(_ context.Context, m Manifest) error {
	if r.err != nil {
		return r.err
	}
	r.items = append(r.items, m)
	return nil
}

type testPolicy struct {
	domains map[string]string
	denied  map[string]bool
}

func (p testPolicy) CheckpointPlacement(_ context.Context, scope Scope, node string) (Placement, error) {
	if p.denied[node] || (scope.Level == "sealed" && scope.HomeNodeID != node) {
		return Placement{}, errors.New("placement denied")
	}
	domain := node
	if p.domains[node] != "" {
		domain = p.domains[node]
	}
	return Placement{FailureDomain: domain}, nil
}

// This transport records the same blob/prepare exchange a node adapter uses,
// while every receiver persists to its own real temporary content directory.
type recordingTransport struct {
	nodes       map[string]*Store
	puts        int
	failPrepare bool
}

func (r *recordingTransport) HasBlob(ctx context.Context, node string, scope Scope, ref BlobRef) (bool, error) {
	if r.nodes[node] == nil {
		return false, errors.New("node offline")
	}
	return r.nodes[node].HasBlob(ctx, scope, ref)
}
func (r *recordingTransport) PutBlob(ctx context.Context, node string, scope Scope, ref BlobRef, content io.Reader) error {
	if r.nodes[node] == nil {
		return errors.New("node offline")
	}
	r.puts++
	return r.nodes[node].PutBlob(ctx, scope, ref, content)
}
func (r *recordingTransport) GetBlob(ctx context.Context, node string, scope Scope, ref BlobRef, into io.Writer) error {
	if r.nodes[node] == nil {
		return errors.New("node offline")
	}
	return r.nodes[node].ReadBlob(ctx, scope, ref, into)
}
func (r *recordingTransport) PrepareCheckpoint(ctx context.Context, node string, snapshot Snapshot) (Receipt, error) {
	if r.nodes[node] == nil {
		return Receipt{}, errors.New("node offline")
	}
	if r.failPrepare {
		return Receipt{}, errors.New("connection closed before receipt")
	}
	return r.nodes[node].Prepare(ctx, snapshot)
}

func newTestStore(t *testing.T, node string, records RecordWriter, transport ReplicaTransport, policy PlacementPolicy, limits Limits) *Store {
	t.Helper()
	s, err := Open(Config{Dir: t.TempDir(), NodeID: node, Records: records, Replicas: transport, Policy: policy, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func testSnapshot(t *testing.T, s *Store, copies int) Snapshot {
	t.Helper()
	scope := Scope{ProjectID: "project-1", Level: "internal", HomeNodeID: "node-a"}
	contextBytes := []byte(`{"goal":"finish validation","findings":["unit tests passed"]}`)
	fileBytes := []byte("package main\n")
	contextRef, fileRef := Reference(contextBytes), Reference(fileBytes)
	for _, item := range []struct {
		ref  BlobRef
		data []byte
	}{{contextRef, contextBytes}, {fileRef, fileBytes}} {
		if err := s.PutBlob(context.Background(), scope, item.ref, bytes.NewReader(item.data)); err != nil {
			t.Fatal(err)
		}
	}
	return Snapshot{Version: 1, Scope: scope,
		Source:  Source{TaskID: "task-1", SessionID: "conversation-1", AttemptID: "attempt-1", TurnID: "turn-1", NodeID: "node-a", ExecutionEpoch: 3},
		Cursors: Cursors{InputAccepted: 4, OutputPublished: 8}, Context: contextRef,
		Workspace:      Workspace{ID: "workspace-1", Files: []File{{Path: "src/main.go", Blob: fileRef}}},
		RequiredCopies: copies, CreatedAt: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)}
}

func TestCommitRequiresWholePackageOnIndependentNodes(t *testing.T) {
	ctx := context.Background()
	records := &recordingRecords{}
	transport := &recordingTransport{nodes: map[string]*Store{}}
	policy := testPolicy{domains: map[string]string{"node-b": "machine-a", "node-a": "machine-a"}}
	a := newTestStore(t, "node-a", records, transport, policy, Limits{})
	b := newTestStore(t, "node-b", nil, nil, policy, Limits{})
	c := newTestStore(t, "node-c", nil, nil, policy, Limits{})
	transport.nodes["node-b"], transport.nodes["node-c"] = b, c
	snapshot := testSnapshot(t, a, 2)
	if _, err := a.Commit(ctx, snapshot, []string{"node-b"}); err == nil {
		t.Fatal("two identities on one machine counted as independent replicas")
	}
	if len(records.items) != 0 {
		t.Fatal("incomplete checkpoint reached ledger")
	}
	transport.failPrepare = true
	if _, err := a.Commit(ctx, snapshot, []string{"node-c"}); err == nil {
		t.Fatal("missing durable receipt accepted")
	}
	if len(records.items) != 0 {
		t.Fatal("interrupted transfer reached ledger")
	}
	transport.failPrepare = false
	m, err := a.Commit(ctx, snapshot, []string{"node-c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records.items) != 1 || len(m.Receipts) != 2 {
		t.Fatalf("commit = %+v", m)
	}
	verified, err := c.Verify(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Manifest().Snapshot.Source.TaskID != "task-1" {
		t.Fatal("task identity lost")
	}
	dest := filepath.Join(t.TempDir(), "recovered")
	if _, err := c.Restore(ctx, m, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "workspace", "src", "main.go"))
	if err != nil || string(data) != "package main\n" {
		t.Fatalf("restored content: %q, %v", data, err)
	}
	puts := transport.puts
	if _, err := a.Commit(ctx, snapshot, []string{"node-c"}); err != nil {
		t.Fatal(err)
	}
	if transport.puts != puts {
		t.Fatal("unchanged content uploaded again")
	}
}

func TestBlobIntegrityBoundariesAndGC(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "node-a", &recordingRecords{}, nil, testPolicy{}, Limits{MaxBlobBytes: 100})
	snapshot := testSnapshot(t, s, 1)
	if err := s.PutBlob(ctx, snapshot.Scope, Reference([]byte("expected")), bytes.NewReader([]byte("corrupt!"))); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	if err := s.PutBlob(ctx, snapshot.Scope, Reference(make([]byte, 101)), bytes.NewReader(make([]byte, 101))); err == nil {
		t.Fatal("blob quota ignored")
	}
	if _, err := s.Commit(ctx, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	orphan := Reference([]byte("orphan"))
	if err := s.PutBlob(ctx, snapshot.Scope, orphan, bytes.NewReader([]byte("orphan"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasBlob(ctx, snapshot.Scope, snapshot.Context); err != nil || !ok {
		t.Fatalf("referenced blob collected: %v", err)
	}
	if ok, err := s.HasBlob(ctx, snapshot.Scope, orphan); err != nil || ok {
		t.Fatalf("orphan not collected: %v", err)
	}
	unsafe := snapshot
	unsafe.Workspace.Files = []File{{Path: "../escape", Blob: snapshot.Workspace.Files[0].Blob}}
	if _, err := s.Commit(ctx, unsafe, nil); err == nil {
		t.Fatal("escaping path accepted")
	}
	sealed := snapshot
	sealed.Scope.Level, sealed.Scope.HomeNodeID = "sealed", "node-other"
	if err := s.PutBlob(ctx, sealed.Scope, orphan, bytes.NewReader([]byte("orphan"))); err == nil {
		t.Fatal("sealed content left home")
	}
}
