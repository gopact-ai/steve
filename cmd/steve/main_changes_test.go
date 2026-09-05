package main

import (
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
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
}
