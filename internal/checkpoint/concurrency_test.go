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

type gatedDownloads struct {
	*recordingTransport
	entered chan struct{}
	ready   chan struct{}
}

func (r *gatedDownloads) GetBlob(ctx context.Context, node string, scope Scope, ref BlobRef, into io.Writer) error {
	r.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.ready:
		return r.recordingTransport.GetBlob(ctx, node, scope, ref, into)
	}
}

func TestReciprocalFetchDoesNotHoldReceivingStoreMutex(t *testing.T) {
	transport := &recordingTransport{nodes: map[string]*Store{}}
	a := newTestStore(t, "node-a", &recordingRecords{}, transport, testPolicy{}, Limits{})
	b := newTestStore(t, "node-b", nil, transport, testPolicy{}, Limits{})
	transport.nodes["node-a"], transport.nodes["node-b"] = a, b
	snapshot := testSnapshot(t, a, 2)
	m, err := a.Commit(context.Background(), snapshot, []string{"node-b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.cfg.Dir, blobName(snapshot.Scope, snapshot.Context))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.cfg.Dir, blobName(snapshot.Scope, snapshot.Workspace.Files[0].Blob))); err != nil {
		t.Fatal(err)
	}
	gated := &gatedDownloads{recordingTransport: transport, entered: make(chan struct{}, 2), ready: make(chan struct{})}
	a.cfg.Replicas, b.cfg.Replicas = gated, gated
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, store := range []*Store{a, b} {
		go func() { _, err := store.Fetch(ctx, m); results <- err }()
	}
	for range 2 {
		select {
		case <-gated.entered:
		case <-ctx.Done():
			t.Fatal("both downloads did not start")
		}
	}
	close(gated.ready)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("reciprocal fetch failed: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("reciprocal fetch deadlocked")
		}
	}
}

func TestCanceledFetchClosesPipeAndReleasesQuotaReservation(t *testing.T) {
	transport := &recordingTransport{nodes: map[string]*Store{}}
	a := newTestStore(t, "node-a", &recordingRecords{}, nil, testPolicy{}, Limits{})
	snapshot := testSnapshot(t, a, 1)
	m, err := a.Commit(context.Background(), snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport.nodes["node-a"] = a
	gated := &gatedDownloads{recordingTransport: transport, entered: make(chan struct{}, 4), ready: make(chan struct{})}
	b := newTestStore(t, "node-b", nil, gated, testPolicy{}, Limits{MaxBytes: 200})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := b.Fetch(ctx, m); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled fetch: %v", err)
	}
	data := bytes.Repeat([]byte("x"), 200)
	if err := b.PutBlob(context.Background(), snapshot.Scope, Reference(data), bytes.NewReader(data)); err != nil {
		t.Fatalf("cancelled stream retained quota: %v", err)
	}
}

func TestFetchAtomicallyRepairsCorruptLocalContent(t *testing.T) {
	ctx := context.Background()
	transport := &recordingTransport{nodes: map[string]*Store{}}
	a := newTestStore(t, "node-a", &recordingRecords{}, transport, testPolicy{}, Limits{})
	b := newTestStore(t, "node-b", nil, transport, testPolicy{}, Limits{})
	transport.nodes["node-a"], transport.nodes["node-b"] = a, b
	snapshot := testSnapshot(t, a, 2)
	m, err := a.Commit(ctx, snapshot, []string{"node-b"})
	if err != nil {
		t.Fatal(err)
	}
	ref := snapshot.Context
	if err := os.WriteFile(filepath.Join(b.cfg.Dir, blobName(snapshot.Scope, ref)), bytes.Repeat([]byte("x"), int(ref.Size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.PutBlob(ctx, snapshot.Scope, ref, bytes.NewReader([]byte("invalid replacement"))); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("invalid repair accepted: %v", err)
	}
	if _, err := b.Fetch(ctx, m); err != nil {
		t.Fatalf("healthy remote copy could not repair local corruption: %v", err)
	}
	if _, err := b.Verify(ctx, m); err != nil {
		t.Fatal(err)
	}
}
