package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestChangesReportsCaptureFailureWithNoArtifact(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	cause := artifact.TooLarge{Which: "files", Have: 3, Limit: 2}.Error()
	record := attempt.Record{Spec: attempt.Spec{ID: "turn", Project: "project", Base: "before"}, Result: &attempt.Result{Summary: "done", CaptureError: cause}}
	if _, err := book.Begin(t.Context(), record.ID, "attempt", string(attempt.Bound), "test", record); err != nil {
		t.Fatal(err)
	}
	admin := &fleetAdmin{attempts: attempt.New(book), artifacts: &artifact.Store{}}
	summary, err := admin.Changes(t.Context(), record.ID)
	if err != nil || summary == nil || summary.Note != "改动没有记录："+cause || summary.Artifact != "" || summary.Files != 0 {
		t.Fatalf("reply did not expose the capture failure: %+v, %v", summary, err)
	}
	if summary.Added != nil || summary.Deleted != nil || summary.BinaryFiles != nil {
		t.Fatal("unknown capture counts must not appear as zero")
	}
}

func TestChangesSummaryKeepsHistoricalSnapshotAndPartialCounts(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	work := t.TempDir()
	projects := project.Open(book)
	p := project.Project{ID: "p", Home: project.Home{Path: work}, Level: project.LevelPublic}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	artifacts := artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", []byte("old\n"))
	before, _, err := artifacts.SnapshotCanonical(t.Context(), p, "", "test", "before")
	if err != nil {
		t.Fatal(err)
	}
	write("a.txt", []byte("new\nsecond\n"))
	write("b.bin", []byte{0, 1, 2, 0})
	write("z.txt", []byte("outside the review limit\n"))
	after, _, err := artifacts.SnapshotCanonical(t.Context(), p, before.ID, "test", "historical result")
	if err != nil {
		t.Fatal(err)
	}
	write("a.txt", []byte("unrelated newer work\n"))
	latest, _, err := artifacts.SnapshotCanonical(t.Context(), p, after.ID, "test", "newer result")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []attempt.Record{
		{Spec: attempt.Spec{ID: "historical", TaskID: "1", Project: p.ID, Base: before.ID}, Result: &attempt.Result{Artifact: after.ID}},
		{Spec: attempt.Spec{ID: "latest", TaskID: "1", Project: p.ID, Base: after.ID}, Result: &attempt.Result{Artifact: latest.ID}},
	} {
		if _, err := book.Begin(t.Context(), record.ID, "attempt", string(attempt.Bound), "test", record); err != nil {
			t.Fatal(err)
		}
	}
	artifacts.Review = artifact.ReviewLimits{MaxChanges: 2}
	admin := &fleetAdmin{attempts: attempt.New(book), artifacts: artifacts}
	summary, err := admin.Changes(t.Context(), "historical")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Attempt != "historical" || summary.Base != before.ID || summary.Artifact != after.ID || !summary.Truncated || summary.Files != 2 {
		t.Fatalf("summary lost historical identity or bounded-count meaning: %+v", summary)
	}
	if summary.Added == nil || *summary.Added != 2 || summary.Deleted == nil || *summary.Deleted != 1 || summary.BinaryFiles == nil || *summary.BinaryFiles != 1 {
		t.Fatalf("summary mixed a newer attempt, omitted binary count or included paths beyond the index: %+v", summary)
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["added"] != float64(2) || body["deleted"] != float64(1) || body["binary_files"] != float64(1) || body["truncated"] != true {
		t.Fatalf("invalid summary response: %s", raw)
	}
}
