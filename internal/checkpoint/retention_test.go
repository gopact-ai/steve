package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedUploadSurvivesGCAndRestartUntilExplicitCollection(t *testing.T) {
	s := newTestStore(t, "node-a", nil, nil, testPolicy{}, Limits{})
	data := []byte("acknowledged without any ledger record")
	retained := RetainedBlob{Sequence: 1, ID: Reference([]byte("owner")).SHA256, Scope: Scope{ProjectID: "p", HomeNodeID: "node-a", Level: "internal"}, Blob: Reference(data)}
	if _, err := s.PutRetainedBlob(t.Context(), retained, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ok, err := reopened.HasBlob(t.Context(), retained.Scope, retained.Blob); err != nil || !ok {
		t.Fatalf("lost unknown acknowledged upload: %v", err)
	}
	alias := retained
	alias.Sequence = 2
	alias.ID = Reference([]byte("second owner")).SHA256
	if _, err := reopened.PutRetainedBlob(t.Context(), alias, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	result, err := reopened.CollectRetained(t.Context(), []RetainedBlob{retained}, nil)
	if err != nil || result.Blobs != 0 {
		t.Fatalf("shared object was collected: %+v %v", result, err)
	}
	result, err = reopened.CollectRetained(t.Context(), []RetainedBlob{alias}, nil)
	if err != nil || result.Blobs != 1 || result.Bytes != int64(len(data)) {
		t.Fatalf("last owner collection=%+v %v", result, err)
	}
	result, err = reopened.CollectRetained(t.Context(), []RetainedBlob{alias, retained}, nil)
	if err != nil || result.Blobs != 0 {
		t.Fatalf("repeated collection=%+v %v", result, err)
	}
	if _, err := reopened.PutRetainedBlob(t.Context(), alias, bytes.NewReader(data)); !errors.Is(err, ErrRetired) {
		t.Fatalf("retired receipt identity resurrected: %v", err)
	}
}

type heldRetainedReader struct {
	entered chan struct{}
	release chan struct{}
	reader  io.Reader
}

func (r *heldRetainedReader) Read(p []byte) (int, error) {
	if r.entered != nil {
		close(r.entered)
		r.entered = nil
		<-r.release
	}
	return r.reader.Read(p)
}

func TestRetainedPutRacingCollectionCannotReturnAnUnprotectedReceipt(t *testing.T) {
	s := newTestStore(t, "node-a", nil, nil, testPolicy{}, Limits{})
	data := []byte("racing upload")
	r := RetainedBlob{Sequence: 1, ID: Reference([]byte("owner")).SHA256, Scope: Scope{ProjectID: "p", HomeNodeID: "node-a", Level: "internal"}, Blob: Reference(data)}
	if _, err := s.PutRetainedBlob(t.Context(), r, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	reader := &heldRetainedReader{entered: make(chan struct{}), release: make(chan struct{}), reader: bytes.NewReader(data)}
	entered := reader.entered
	done := make(chan error, 1)
	go func() { _, err := s.PutRetainedBlob(context.Background(), r, reader); done <- err }()
	<-entered
	result, err := s.CollectRetained(t.Context(), []RetainedBlob{r}, nil)
	close(reader.release)
	if err != nil || result.Blobs != 0 {
		t.Fatalf("GC failed or removed in-flight upload: %+v %v", result, err)
	}
	if err := <-done; !errors.Is(err, ErrRetired) {
		t.Fatalf("retired concurrent upload was acknowledged: %v", err)
	}
	if result, err = s.CollectRetained(t.Context(), []RetainedBlob{r}, nil); err != nil || result.Blobs != 1 {
		t.Fatalf("retry did not collect completed unacknowledged upload: %+v %v", result, err)
	}
}

func TestRetainedGCValidatesEveryMarkerBeforeDeletingAnything(t *testing.T) {
	s := newTestStore(t, "node-a", nil, nil, testPolicy{}, Limits{})
	data := []byte("protected")
	r := RetainedBlob{Sequence: 1, ID: Reference([]byte("owner")).SHA256, Scope: Scope{ProjectID: "p", HomeNodeID: "node-a", Level: "internal"}, Blob: Reference(data)}
	if _, err := s.PutRetainedBlob(t.Context(), r, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.Dir, "retained", Reference([]byte("corrupt")).SHA256+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CollectRetained(t.Context(), []RetainedBlob{r}, nil); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt reachability marker ignored: %v", err)
	}
	if ok, err := s.HasBlob(t.Context(), r.Scope, r.Blob); err != nil || !ok {
		t.Fatalf("deleted before validating all protection: %v", err)
	}
}

func TestExactRetentionReleaseWorksAtQuotaAndSurvivesInterruptedRename(t *testing.T) {
	s := newTestStore(t, "node-a", nil, nil, testPolicy{}, Limits{})
	data := bytes.Repeat([]byte("q"), 1024)
	r := RetainedBlob{Sequence: 1, ID: Reference([]byte("receipt-one")).SHA256, Scope: Scope{ProjectID: "p", Level: "internal", HomeNodeID: "node-a"}, Blob: Reference(data)}
	at, err := s.PutRetainedBlob(t.Context(), r, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Limits.MaxBytes = s.used
	if next, err := s.PutRetainedBlob(t.Context(), r, bytes.NewReader(data)); err != nil || !at.Equal(next) {
		t.Fatalf("idempotent receipt at quota: %v", err)
	}
	other := r
	other.Sequence = 2
	other.ID = Reference([]byte("unknown-new-receipt")).SHA256
	if _, err := s.PutRetainedBlob(t.Context(), other, bytes.NewReader(data)); !errors.Is(err, ErrQuota) {
		t.Fatalf("metadata quota ignored: %v", err)
	}
	if _, err := s.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasBlob(t.Context(), r.Scope, r.Blob); err != nil || !ok {
		t.Fatalf("quota evicted unknown: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate interruption after the durable release rename, before unlink.
	if err := os.Rename(filepath.Join(s.cfg.Dir, "retained", r.ID+".json"), filepath.Join(s.cfg.Dir, "retired", r.ID+".json")); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.CollectRetained(t.Context(), []RetainedBlob{r}, nil)
	if err != nil || result.Blobs != 1 || reopened.used != retentionFloorBytes || reopened.objects != 1 {
		t.Fatalf("recovery/accounting=%+v used=%d err=%v", result, reopened.used, err)
	}
	if _, err := reopened.PutRetainedBlob(t.Context(), r, bytes.NewReader(data)); !errors.Is(err, ErrRetired) {
		t.Fatalf("released replay after restart: %v", err)
	}
	result, err = reopened.CollectRetained(t.Context(), []RetainedBlob{r}, nil)
	if err != nil || result.Blobs != 0 {
		t.Fatalf("repeated GC double accounting: %+v %v", result, err)
	}
}
