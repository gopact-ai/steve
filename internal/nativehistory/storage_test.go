package nativehistory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStorageBoundsConcurrentImportsAndKeepsCompletedRetries(t *testing.T) {
	home, store := t.TempDir(), t.TempDir()
	writeFixture(t, home, "sessions/rollout-one.jsonl", `{"type":"session_meta","payload":{"id":"one","cwd":"/work"}}`+"\n")
	src := Source{Harness: "codex", Home: home}
	entries, err := List(t.Context(), src)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %+v %v", entries, err)
	}
	request := ImportRequest{CommandID: "retry", Source: src, NativeID: "one", Revision: entries[0].Revision, Workdir: "/work"}
	ref, err := Snapshot(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxStoreEntries-2; i++ {
		writeFixture(t, store, fmt.Sprintf("retained-%d/file", i), "history")
	}
	var wg sync.WaitGroup
	errorsOut := make(chan error, 4)
	for i := range 4 {
		wg.Go(func() {
			req := request
			req.CommandID = fmt.Sprint(i)
			_, err := Snapshot(t.Context(), store, req)
			errorsOut <- err
		})
	}
	wg.Wait()
	close(errorsOut)
	success := 0
	for err := range errorsOut {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrStorageFull) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent admission accepted %d imports", success)
	}
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	again, err := Snapshot(t.Context(), store, request)
	if err != nil || again != ref {
		t.Fatal("full store lost completed import retry", err)
	}
}

func TestStorageCountsAbandonedStagesAndBytesWithoutFollowingLinks(t *testing.T) {
	store := t.TempDir()
	release, err := LockStorage(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	path := filepath.Join(store, ".import-abandoned", "history")
	writeFixture(t, store, ".import-abandoned/history", "")
	if err := os.Truncate(path, MaxStoreBytes); err != nil {
		t.Fatal(err)
	}
	if err := CheckStorage(t.Context(), store, 1, 1); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("ignored abandoned bytes: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := LockStorage(ctx, store); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock did not respect cancellation: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(store, "access")); err != nil {
		t.Fatal(err)
	}
	if err := CheckStorage(t.Context(), store, 1, MaxSnapshotBytes); err != nil {
		t.Fatal(err)
	}
}

func TestMismatchedDestinationNeverCopiesHistory(t *testing.T) {
	home, store := t.TempDir(), t.TempDir()
	writeFixture(t, home, "sessions/rollout-one.jsonl", `{"type":"session_meta","payload":{"id":"one","cwd":"/work"}}`+"\n")
	src := Source{Harness: "codex", Home: home}
	entries, err := List(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Snapshot(t.Context(), store, ImportRequest{CommandID: "bad", Source: src, NativeID: "one", Revision: entries[0].Revision, Workdir: "/other"}); err == nil {
		t.Fatal("mismatched destination imported")
	}
	files, err := os.ReadDir(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name() != ".admission.lock" {
			t.Fatal("rejected import copied files", f.Name())
		}
	}
}
