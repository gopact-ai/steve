package turn

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/state"
)

func TestAfterSnapshotFailureKeepsReplyAndCaptureError(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{reply: "done", started: make(chan struct{}), done: make(chan struct{})}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}, time.Minute)
	c.artifacts.Limits = gitrepo.Limits{MaxFiles: 1}
	// The capture failure is recorded in the language of the turn.
	c.text = i18n.New(i18n.LocaleEN)
	work := workspaceOf(t, c, "codex")
	if err := os.WriteFile(filepath.Join(work, "before"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var result Result
	var turnErr error
	go func() {
		defer close(done)
		result, turnErr = handle(c, t.Context(), "hello")
	}()
	select {
	case <-runner.started:
	case <-time.After(waitDeadline):
		t.Fatal("turn did not start")
	}
	// The before-snapshot fits. The turn itself takes the workspace over
	// budget, so only its capture fails and the answer must still arrive.
	if err := os.WriteFile(filepath.Join(work, "after"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(runner.done)
	select {
	case <-done:
	case <-time.After(waitDeadline):
		t.Fatal("turn did not finish")
	}
	if turnErr != nil || result.Text != "done" || result.Attempt == "" {
		t.Fatalf("capture failure lost the reply: %+v, %v", result, turnErr)
	}
	record, err := c.attempts.Get(t.Context(), result.Attempt)
	want := gitrepo.TooLarge{Which: "files", Have: 2, Limit: 1}.Say(c.text)
	if err != nil || record.State != attempt.Bound || record.Result == nil || record.Result.Artifact != "" || record.Result.CaptureError != want {
		t.Fatalf("capture failure was not persisted: %+v, %v", record, err)
	}
	if head, err := c.artifacts.CanonicalOf(t.Context(), record.Project); err != nil || head != record.Base {
		t.Fatalf("failed capture moved the canonical head: %s, %v", head, err)
	}
}
