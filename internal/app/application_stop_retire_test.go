package app

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
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

// confirmedUnprojectedStop has a first pass confirm the fixture's native
// stop but fail to settle its accounting: the stop is confirmed, and
// neither accounted nor marked projected.
func confirmedUnprojectedStop(t *testing.T, f *stopRegistryFixture) attempt.Record {
	t.Helper()
	f.owner.Finish(&execution.RetainedObserverDetached{AttemptID: f.record.ID, NodeID: f.record.Node, SessionID: f.record.Session, Cause: harness.ErrStopUnconfirmed})
	if _, err := f.book.DB().Exec(`CREATE TRIGGER reject_stop_settlement BEFORE INSERT ON bindings WHEN NEW.kind = 'task-attempt' BEGIN SELECT RAISE(FAIL, 'isolated accounting failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.stops.Reconcile(f.ctx); err == nil || !strings.Contains(err.Error(), "accounting remains pending") {
		t.Fatalf("first pass = %v, want its accounting pending", err)
	}
	if _, err := f.book.DB().Exec(`DROP TRIGGER reject_stop_settlement`); err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.attempts.Get(f.ctx, f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.StopEvidence != "task-stop/"+confirmed.ID || confirmed.SessionSettled == nil || !*confirmed.SessionSettled || confirmed.Unsettled || confirmed.StopProjected {
		t.Fatalf("stop is not confirmed and unprojected: %+v", confirmed)
	}
	return confirmed
}

// A stop pass whose context has ended starts none of the writes that
// finish a confirmed stop — accounting, resolution, the projection mark —
// and the next pass finishes them.
func TestApplicationStopEndedPassLeavesConfirmedStopToNextPass(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP peer: %v %s", err, output)
	}
	f := newStopRegistryFixture(t, bin, false)
	confirmed := confirmedUnprojectedStop(t, f)

	ended, cancel := context.WithCancel(f.ctx)
	cancel()
	if err := f.stops.stop(ended, confirmed); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop in an ended pass = %v, want context.Canceled", err)
	}
	current, err := f.attempts.Get(f.ctx, f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.StopProjected {
		t.Fatal("an ended pass marked the stop projected")
	}
	if row, _ := f.tasks.Get(current.TaskID); !row.Attempts[0].Open() {
		t.Fatal("an ended pass settled the stop's accounting")
	}
	f.requireBusy(t)

	if err := f.stops.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	if current, err = f.attempts.Get(f.ctx, f.record.ID); err != nil || !current.StopProjected {
		t.Fatalf("the next pass did not finish the stop: %+v (%v)", current, err)
	}
	f.requireResolved(t)
}

// passEndingReplica stands in for a cluster whose local replica is behind
// at one write of a stop pass: that write's Prepare ends the pass, then
// waits for the replica until the write's ctx ends. A write that does not
// wait under the pass's ctx gets an error after ten seconds instead.
type passEndingReplica struct {
	applicationMCPReplicator
	at       int64
	prepares atomic.Int64
	end      context.CancelFunc
}

var errReplicaOutlivedPass = errors.New("write waited on the replica after its stop pass ended")

func (r *passEndingReplica) Prepare(ctx context.Context) (ledger.ReplicaPosition, error) {
	if r.prepares.Add(1) != r.at {
		return r.applicationMCPReplicator.Prepare(ctx)
	}
	r.end()
	select {
	case <-ctx.Done():
		return ledger.ReplicaPosition{}, ctx.Err()
	case <-time.After(10 * time.Second):
		return ledger.ReplicaPosition{}, errReplicaOutlivedPass
	}
}

// A stop pass that ends while one of its writes, the accounting or the
// projection mark, waits on a lagging replica gets that write back with
// the pass's error rather than waiting for the replica, and the next pass
// finishes the stop.
func TestApplicationStopWritesWaitOnAReplicaOnlyWithinTheirPass(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP peer: %v %s", err, output)
	}
	for _, tc := range []struct {
		name    string
		write   int64 // the pass's write the replica is behind at
		settled bool  // whether the accounting was settled before it
	}{
		{name: "accounting", write: 1},
		{name: "projection mark", write: 2, settled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStopRegistryFixture(t, bin, false)
			confirmed := confirmedUnprojectedStop(t, f)
			pass, end := context.WithCancel(f.ctx)
			defer end()
			if err := f.book.AttachReplication(&passEndingReplica{applicationMCPReplicator: applicationMCPReplicator{f.book}, at: tc.write, end: end}); err != nil {
				t.Fatal(err)
			}
			if err := f.stops.stop(pass, confirmed); !errors.Is(err, context.Canceled) {
				t.Fatalf("stop whose pass ended at write %d = %v, want context.Canceled", tc.write, err)
			}
			current, err := f.attempts.Get(f.ctx, f.record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.StopProjected {
				t.Fatal("an ended pass marked the stop projected")
			}
			if row, _ := f.tasks.Get(current.TaskID); row.Attempts[0].Open() == tc.settled {
				t.Fatalf("accounting open = %v after the pass ended at write %d", row.Attempts[0].Open(), tc.write)
			}
			if !tc.settled {
				f.requireBusy(t)
			}
			if err := f.stops.Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			if current, err = f.attempts.Get(f.ctx, f.record.ID); err != nil || !current.StopProjected {
				t.Fatalf("the next pass did not finish the stop: %+v (%v)", current, err)
			}
			f.requireResolved(t)
		})
	}
}

// heldReplica holds the first write's Prepare, and with it the ledger's
// writer, until release is closed.
type heldReplica struct {
	applicationMCPReplicator
	prepares atomic.Int64
	held     chan struct{}
	release  chan struct{}
}

func (r *heldReplica) Prepare(ctx context.Context) (ledger.ReplicaPosition, error) {
	if r.prepares.Add(1) == 1 {
		close(r.held)
		<-r.release
	}
	return r.applicationMCPReplicator.Prepare(ctx)
}

// A stop pass that has already ended does not queue behind a write that
// holds the ledger: it returns before that write is let go, and writes
// nothing.
func TestApplicationStopEndedPassDoesNotQueueBehindAWriter(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP peer: %v %s", err, output)
	}
	f := newStopRegistryFixture(t, bin, false)
	confirmed := confirmedUnprojectedStop(t, f)
	replica := &heldReplica{applicationMCPReplicator: applicationMCPReplicator{f.book}, held: make(chan struct{}), release: make(chan struct{})}
	if err := f.book.AttachReplication(replica); err != nil {
		t.Fatal(err)
	}
	writer := make(chan error, 1)
	go func() { writer <- f.book.Update(context.Background(), func(*ledger.Tx) error { return nil }) }()
	<-replica.held

	ended, cancel := context.WithCancel(f.ctx)
	cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- f.stops.stop(ended, confirmed) }()
	var err error
	select {
	case err = <-stopped:
	case <-time.After(10 * time.Second):
		t.Error("an ended stop pass queued behind the held writer")
	}
	close(replica.release)
	if writeErr := <-writer; writeErr != nil {
		t.Fatal(writeErr)
	}
	if t.Failed() {
		err = <-stopped
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stop in an ended pass = %v, want context.Canceled", err)
	}
	if current, _ := f.tasks.Get(confirmed.TaskID); !current.Attempts[0].Open() {
		t.Fatal("an ended pass settled the stop's accounting")
	}
}
