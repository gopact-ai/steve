package app

import (
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
)

func stopCandidateIDs(t *testing.T, f *stopRegistryFixture) []string {
	t.Helper()
	records, err := f.attempts.StopCandidates(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	return ids
}

// Once a confirmed task stop's accounting is durably projected, the stop
// candidate index no longer holds it: pause and cancel history does not
// grow every later pass. An owner that joins afterwards is still resolved
// from the registry, and a confirmed stop recorded without the projection
// mark is checked against its accounting once and then retired.
func TestApplicationStopRetiresProjectedStopsFromCandidates(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP peer: %v %s", err, output)
	}
	f := newStopRegistryFixture(t, bin, false)
	if err := f.stops.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	current, err := f.attempts.Get(f.ctx, f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.StopEvidence != "task-stop/"+current.ID || !current.StopProjected || attempt.TaskStopOwed(current) {
		t.Fatalf("projected stop was not retired: %+v", current)
	}
	if ids := stopCandidateIDs(t, f); slices.Contains(ids, current.ID) {
		t.Fatalf("projected stop still a candidate: %v", ids)
	}
	f.requireBusy(t)
	calls := f.sessions.calls.Load()
	f.owner.Finish(harness.ErrStopUnconfirmed)
	if err := f.stops.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.sessions.calls.Load() != calls {
		t.Fatal("resolving a late owner contacted the node again")
	}
	if active := f.registry.Active(); len(active) != 0 {
		t.Fatalf("late owner of a retired stop was not resolved: %v", active)
	}

	if _, err := f.book.DB().Exec(`UPDATE operations SET data=json_remove(data,'$.stop_projected') WHERE id=?`, current.ID); err != nil {
		t.Fatal(err)
	}
	if ids := stopCandidateIDs(t, f); !slices.Contains(ids, current.ID) {
		t.Fatalf("unmarked confirmed stop missing from candidates: %v", ids)
	}
	if _, err := f.book.DB().Exec(`CREATE TRIGGER reject_repeated_accounting BEFORE INSERT ON bindings WHEN NEW.kind = 'task-attempt' BEGIN SELECT RAISE(FAIL, 'accounting must not be repeated'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.stops.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.sessions.calls.Load() != calls {
		t.Fatal("checking recorded accounting contacted the node again")
	}
	if ids := stopCandidateIDs(t, f); slices.Contains(ids, current.ID) {
		t.Fatalf("already accounted stop was not retired: %v", ids)
	}
}
