package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func runtimeUsageFixture(t *testing.T) (*Store, RuntimeRecord) {
	t.Helper()
	store, d := configuredDeployment(t)
	receipt, err := store.PrepareDeployment(t.Context(), d, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	selection := Selection{Project: "p", Node: "worker", Harness: "mock", Deployments: []string{receipt.Hash}}
	record, err := store.PrepareRuntime(t.Context(), "runtime-use", selection, RuntimeConfig{Command: "unused"}, func(context.Context, string, RuntimeRecord) (string, error) { return contentDigest(nil), nil })
	if err != nil {
		t.Fatal(err)
	}
	return store, record
}

func TestRuntimeRemovalResumesAfterPartialDirectoryDeletion(t *testing.T) {
	store, record := runtimeUsageFixture(t)
	if err := store.RetireRuntime(t.Context(), record.Ref); err != nil {
		t.Fatal(err)
	}
	// Deletion can remove the retirement marker before failing on another
	// entry; the separately persisted removal marker still closes admission.
	if err := os.WriteFile(filepath.Join(store.Dir, "removed-runtimes", record.Ref.ID), []byte(`{"removed":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.RuntimeDir(record.Ref.ID), "retired.json")); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRuntimeUse(t.Context(), record.Ref, "late", "host"); !errors.Is(err, ErrRuntimeRetired) {
		t.Fatalf("partial removal reopened admission: %v", err)
	}
	if err := store.RemoveRuntime(t.Context(), record.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.RuntimeDir(record.Ref.ID)); !os.IsNotExist(err) {
		t.Fatalf("partial removal could not finish: %v", err)
	}
}

func TestRuntimeRemovalCanRetireInterruptedPreparation(t *testing.T) {
	store, record := runtimeUsageFixture(t)
	if err := os.RemoveAll(store.RuntimeDir(record.Ref.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireRuntime(t.Context(), record.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveRuntime(t.Context(), record.Ref); err != nil {
		t.Fatal(err)
	}
	_, _, err := store.ResumeRuntimePreparation(t.Context(), record.CommandID, record.Ref.Selection, func(context.Context, string, RuntimeRecord) (string, error) {
		t.Fatal("removed preparation was recreated")
		return "", nil
	})
	if !errors.Is(err, ErrRuntimeRetired) {
		t.Fatalf("interrupted command survived removal: %v", err)
	}
}

func TestRuntimeRemovalRequiresRetirementAndPositiveStopEvidence(t *testing.T) {
	store, record := runtimeUsageFixture(t)
	if err := store.BeginRuntimeUse(t.Context(), record.Ref, "process-one", "host"); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveRuntime(t.Context(), record.Ref); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("removed active runtime: %v", err)
	}
	if err := store.RetireRuntime(t.Context(), record.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRuntimeUse(t.Context(), record.Ref, "process-two", "host"); !errors.Is(err, ErrRuntimeRetired) {
		t.Fatalf("retired runtime admitted process: %v", err)
	}
	if err := store.RemoveRuntime(t.Context(), record.Ref); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("retirement implied process stopped: %v", err)
	}
	reopened := &Store{Dir: store.Dir}
	infos, err := reopened.RuntimeInfos()
	if err != nil || len(infos) != 1 || len(infos[0].Uses) != 1 || infos[0].Uses[0].Stopped {
		t.Fatalf("restart lost use: %+v %v", infos, err)
	}
	if err := reopened.EndRuntimeUse(t.Context(), record.Ref, "process-one"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RemoveRuntime(t.Context(), record.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.RuntimeDir(record.Ref.ID)); !os.IsNotExist(err) {
		t.Fatal("runtime directory remains")
	}
	if _, err := reopened.PrepareRuntime(t.Context(), record.CommandID, record.Ref.Selection, record.Config, func(context.Context, string, RuntimeRecord) (string, error) {
		t.Fatal("recreated removed runtime")
		return "", nil
	}); !errors.Is(err, ErrRuntimeRetired) {
		t.Fatalf("removed command resurrected: %v", err)
	}
}
