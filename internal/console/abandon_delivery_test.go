package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

type durableAbandonDriver struct {
	directory string
	attempts  *attempt.Service
	tasks     *task.Store
	sessions  *state.Store
}

func (d *durableAbandonDriver) AbandonAttempt(ctx context.Context, id, owner string, revision uint64) (attempt.Record, error) {
	r, err := d.attempts.Get(ctx, id)
	if err != nil {
		return r, err
	}
	err = d.tasks.AbandonExecution(ctx, r.TaskID, r.ID, r.TurnID, func(tx *ledger.Tx, row task.Attempt, at time.Time) (task.RecoveryUsage, error) {
		return d.attempts.AbandonTx(tx, id, owner, revision, row, at, attempt.AbandonContext{State: "absent"})
	})
	if err != nil && !errors.Is(err, attempt.ErrAlreadyAbandoned) {
		return attempt.Record{}, err
	}
	return d.attempts.Get(ctx, id)
}
func (d *durableAbandonDriver) ProjectAbandoned(ctx context.Context, id string) error {
	r, err := d.attempts.Get(ctx, id)
	if err != nil {
		return err
	}
	if !r.Abandoned.ProjectedAt.IsZero() {
		return nil
	}
	owed := state.OwedClose{NodeID: r.Node, HarnessID: r.Harness, UpstreamID: r.Session, TaskID: r.TaskID, AttemptID: r.ID, OwedAt: r.Abandoned.At.Format(time.RFC3339Nano)}
	if err := d.sessions.ProjectAbandonedSession(ctx, r.Abandoned.Conversation, r.Agent, owed, true, "", func(tx *ledger.Tx) error { return d.attempts.CheckAbandonProjectionTx(tx, r) }); err != nil {
		return err
	}
	_, err = d.attempts.ProjectAbandonedCapacity(ctx, id, r.Abandoned.ForceStopRevision)
	return err
}
func (d *durableAbandonDriver) PendingAbandonments(ctx context.Context) ([]attempt.Record, error) {
	return d.attempts.AbandonProjections(ctx)
}
func (d *durableAbandonDriver) ReadAbandoned(ctx context.Context, id string) (attempt.Record, error) {
	return d.attempts.Get(ctx, id)
}
func (d *durableAbandonDriver) CompleteAbandonDelivery(ctx context.Context, r attempt.Record, proof func(ledger.Reader, attempt.Record) (attempt.AbandonDelivery, error)) error {
	return d.attempts.CompleteAbandonDelivery(ctx, r, proof)
}

func durableAbandonFixture(t *testing.T) (*Service, *ledger.Ledger, *durableAbandonDriver, attempt.Record) {
	return durableAbandonFixtureAt(t, attempt.KindChat, "console:delivery")
}

func durableAbandonFixtureAt(t *testing.T, kind attempt.Kind, conversation string) (*Service, *ledger.Ledger, *durableAbandonDriver, attempt.Record) {
	t.Helper()
	directory := t.TempDir()
	book, err := ledger.Open(directory, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	d := &durableAbandonDriver{directory: directory, attempts: attempt.New(book), tasks: tasks, sessions: sessions}
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: conversation, AnchorMessage: "web-original", Member: "worker", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(tracked.ID)
	r, err := d.attempts.Open(t.Context(), attempt.Spec{ID: "abandon-delivery", TaskID: tracked.ID, TurnID: "web-original", Execution: &token, Kind: kind, Agent: "worker", Node: "node", Harness: "mock", Project: "p", Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "original", Project: "p", Node: "node", Path: t.TempDir(), Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = d.attempts.Advance(t.Context(), r.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_delivery" })
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tasks.SetAside(tracked.ID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := d.attempts.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	r, err = d.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven")
	if err != nil {
		t.Fatal(err)
	}
	s := New(&echo{}, "owner", nil)
	s.EnableRetainedRecovery(t.Context())
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	s.SetAbandons(d)
	e := &queuedExchange{Exchange: Exchange{ID: "original", Conversation: tracked.Channel, State: consoleapi.ExchangeAwaitingUser, ExpectedProject: "p", Input: "original work"}, RecoveryStopPending: "unproved", done: make(chan struct{})}
	s.mu.Lock()
	s.exchanges[tracked.Channel] = []*queuedExchange{e}
	s.meta[tracked.Channel] = Meta{Title: "original conversation"}
	s.running[tracked.Channel] = 1
	err = s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return s, book, d, r
}
func abandonDeliverySQL(t *testing.T, book *ledger.Ledger, statement string) {
	t.Helper()
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(statement); return err }); err != nil {
		t.Fatal(err)
	}
}
func TestAbandonmentDeliveryIsRetriedAfterSessionProjectionCommitted(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same process", true: "restart"}[restart], func(t *testing.T) {
			s, book, d, r := durableAbandonFixture(t)
			abandonDeliverySQL(t, book, `CREATE TRIGGER refuse_abandon_receipt BEFORE UPDATE ON bindings WHEN NEW.kind='console-exchange' AND json_extract(NEW.data,'$.recovery_abandon') IS NOT NULL BEGIN SELECT RAISE(ABORT,'receipt refused'); END`)
			out, err := s.Abandon(t.Context(), r.ID, 1)
			if err != nil || !out.Accepted || !out.Pending {
				t.Fatalf("accepted pending=%+v %v", out, err)
			}
			current, _ := d.attempts.Get(t.Context(), r.ID)
			if current.Abandoned.ProjectedAt.IsZero() {
				t.Fatal("test did not pass session projection")
			}
			if current.Abandoned.DeliveryDoneAt.IsZero() == false {
				t.Fatal("failed receipt was acknowledged")
			}
			if s.exchanges["console:delivery"][0].RecoveryAbandon != nil || len(s.Replies("delivery")) != 0 {
				t.Fatal("refused receipt leaked into memory")
			}
			abandonDeliverySQL(t, book, `DROP TRIGGER refuse_abandon_receipt`)
			if restart {
				s, book = reopenAbandonReceiver(t, book, d)
			}
			if err := s.ReconcileAbandonments(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := s.ReconcileAbandonments(t.Context()); err != nil {
				t.Fatal(err)
			}
			replies := s.Replies("delivery")
			if len(replies) != 1 || replies[0].AttemptID != r.ID {
				t.Fatalf("original abandonment reply was lost or repeated: %+v", replies)
			}
			current, _ = d.attempts.Get(t.Context(), r.ID)
			if current.Abandoned.DeliveryDoneAt.IsZero() || current.Abandoned.DeliveryResult != attempt.AbandonDelivered {
				t.Fatalf("delivery evidence missing: %+v", current.Abandoned)
			}
			pending, err := d.PendingAbandonments(t.Context())
			if err != nil || len(pending) != 0 {
				t.Fatalf("settled delivery remains pending: %v %v", pending, err)
			}
		})
	}
}

// A refusal of the final reply must not leave a terminal input in memory.
func TestAbandonmentFinalReplyFailureRollsBackBeforePublishing(t *testing.T) {
	s, book, d, r := durableAbandonFixture(t)
	life, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.EnableRetainedRecovery(life)
	replica := &abandonReplyReplica{book: book, failed: make(chan struct{})}
	replica.reject.Store(true)
	if err := book.AttachReplication(replica); err != nil {
		t.Fatal(err)
	}
	type result struct {
		out consoleapi.Abandonment
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := s.Abandon(t.Context(), r.ID, 1); done <- result{out, err} }()
	select {
	case <-replica.failed:
	case <-time.After(3 * time.Second):
		t.Fatal("the final reply was never proposed")
	}
	cancel()
	var got result
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("abandonment could not return after a failed reply")
	}
	if got.err != nil || !got.out.Accepted || !got.out.Pending {
		t.Fatalf("failed final reply claimed completed delivery: %+v %v", got.out, got.err)
	}
	s.mu.Lock()
	e := s.exchanges["console:delivery"][0]
	terminal := e.State.Terminal()
	receipt := e.RecoveryAbandon
	receipts := len(s.replies[e.Conversation])
	s.mu.Unlock()
	if terminal || receipt != nil || receipts != 0 {
		t.Fatalf("failed final save leaked state: terminal=%v receipt=%v replies=%d", terminal, receipt, receipts)
	}
	current, _ := d.attempts.Get(t.Context(), r.ID)
	if !current.Abandoned.DeliveryDoneAt.IsZero() {
		t.Fatal("failed final save was acknowledged")
	}
	replica.reject.Store(false)
	for _, service := range []*Service{s} {
		service.EnableRetainedRecovery(t.Context())
		if err := service.ReconcileAbandonments(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Replies("delivery")) != 1 {
		t.Fatal("recovered delivery was not appended exactly once")
	}
	saved, err := LoadState(book)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(saved)
	if len(raw) == 0 {
		t.Fatal("missing committed receipt state")
	}
}

type abandonReplyReplica struct {
	book   *ledger.Ledger
	failed chan struct{}
	once   sync.Once
	reject atomic.Bool
}

func (r *abandonReplyReplica) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}
func (r *abandonReplyReplica) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	if r.reject.Load() && bytes.Contains(write.Payload, []byte("console-reply")) {
		r.once.Do(func() { close(r.failed) })
		return nil, errors.New("final abandonment reply refused")
	}
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

func reopenAbandonReceiver(t *testing.T, book *ledger.Ledger, d *durableAbandonDriver) (*Service, *ledger.Ledger) {
	t.Helper()
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := ledger.Open(d.directory, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	d.attempts = attempt.New(opened)
	d.tasks, err = task.OpenLedger(opened)
	if err != nil {
		t.Fatal(err)
	}
	d.sessions, err = state.OpenLedger(opened)
	if err != nil {
		t.Fatal(err)
	}
	s := New(&echo{}, "owner", nil)
	s.EnableRetainedRecovery(t.Context())
	if err := s.PersistLedger(opened); err != nil {
		t.Fatal(err)
	}
	s.SetAbandons(d)
	return s, opened
}
