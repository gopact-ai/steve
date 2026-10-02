package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

type recoverySnapshotGate struct {
	artifact.LocalNodes
	entered        chan struct{}
	release        chan struct{}
	once           sync.Once
	enabled        atomic.Bool
	blockOp        ops.Kind
	applyCalls     atomic.Int64
	snapshotCommit string
}

func (n *recoverySnapshotGate) Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	if req.Op == ops.Apply {
		n.applyCalls.Add(1)
	}
	op := n.blockOp
	if op == "" {
		op = ops.Snapshot
	}
	if req.Op == op && n.enabled.Load() {
		n.once.Do(func() { close(n.entered) })
		select {
		case <-n.release:
		case <-ctx.Done():
			return ops.Result{}, ctx.Err()
		}
	}
	result, err := n.LocalNodes.Artifact(ctx, node, req)
	if req.Op == ops.Snapshot && err == nil {
		n.snapshotCommit = result.Commit
	}
	return result, err
}

func recoveryCanonicalWriterFixture(t *testing.T, gate *recoverySnapshotGate, quarantine bool) (*Coordinator, project.Project, attempt.Record, string) {
	t.Helper()
	c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
	c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, gate)
	p := project.Project{ID: "recovery", Home: project.Home{Node: "node", Path: t.TempDir()}}
	if err := c.projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = c.projects.Get(t.Context(), p.ID)
	if err := os.WriteFile(filepath.Join(p.Home.Path, "original"), []byte("named base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var base string
	{
		m, _, err := c.artifacts.SnapshotCanonical(t.Context(), p, "", "fixture", "before original work")
		if err != nil {
			t.Fatal(err)
		}
		base = m.ID
	}
	tracked, err := tasks.Create(task.Task{Channel: "console:recovery", Transport: "console", Member: "worker", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(tracked.ID)
	r, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "source", Kind: attempt.KindChat, TaskID: tracked.ID, TurnID: "web-original", Execution: &token, Project: p.ID, Node: "node", Harness: "mock", Agent: "worker", Base: base, Scope: attempt.ScopeUnrestricted, Workspace: p.Canonical()})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = c.attempts.Advance(t.Context(), r.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_source" })
		if err != nil {
			t.Fatal(err)
		}
	}
	if !quarantine {
		return c, p, r, base
	}
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "fixture", errors.New("original writer unreachable"), nil); err != nil {
		t.Fatal(err)
	}
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	r, err = c.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven")
	if err != nil {
		t.Fatal(err)
	}
	return c, p, r, base
}

func TestRecoveryCanonicalSnapshotRequiresFinalAcceptance(t *testing.T) {
	gate := &recoverySnapshotGate{LocalNodes: artifact.LocalNodes{Dir: t.TempDir()}, entered: make(chan struct{}), release: make(chan struct{})}
	c, p, source, base := recoveryCanonicalWriterFixture(t, gate, false)
	snapshots := c.artifacts
	gate.enabled.Store(true)
	held, ok := artifact.CanonicalLease(source.Leases, p.ID)
	if !ok {
		t.Fatal("fixture has no original canonical lease")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type outcome struct {
		manifest artifact.Manifest
		err      error
	}
	done := make(chan outcome, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m, _, err := snapshots.SnapshotCanonicalUnder(ctx, p, held, base, source.ID, "old writer result")
		done <- outcome{m, err}
	}()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
		wg.Wait()
	}()
	select {
	case <-gate.entered:
	case got := <-done:
		t.Fatalf("snapshot never reached the controlled dispatch: %v", got.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.attempts.MarkUnsettled(ctx, source.ID, "fixture", errors.New("writer became unreachable during snapshot"), nil); err != nil {
		t.Fatal(err)
	}
	if err := NewForceStopControl(c).ForceStopAttempt(ctx, source.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.RecordForceStopResult(ctx, source.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
	abandoned, err := NewAbandonControl(c).AbandonAttempt(ctx, source.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Home.Path, "original"), []byte("quarantined late bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	got := <-done
	if !errors.Is(got.err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("canonical-name hold did not refuse: %v", got.err)
	}
	pinned, err := c.attempts.WorkspaceRecovery(ctx, abandoned.Abandoned.WorkspaceRecoveryID)
	if err != nil || pinned.Baseline.Artifact != base {
		t.Fatalf("baseline changed: %+v %v", pinned, err)
	}
	head, _ := c.artifacts.CanonicalOf(ctx, p.ID)
	if head != base {
		t.Fatalf("canonical name changed despite hold: %s", head)
	}
	candidate := gate.snapshotCommit
	if candidate == "" || candidate == base {
		t.Fatal("fixture did not produce a late candidate")
	}
	if _, found, err := c.artifacts.Manifest(ctx, candidate); err != nil || found {
		t.Errorf("hold rejected name but accepted late canonical artifact: found=%v err=%v", found, err)
	}
	if err := ledgerOf(t, c).Read(ctx, func(tx *ledger.ReadTx) error {
		_, err := c.artifacts.RecoveryOutputTx(tx, p.ID, base, candidate)
		return err
	}); err == nil {
		t.Error("recovery validator accepted rejected canonical candidate")
	}
	for _, asInput := range []bool{false, true} {
		req := project.Request{Project: p.ID, Isolated: true, Base: candidate, Owner: "late-candidate"}
		if asInput {
			req.Base = base
			req.Owner = "late-input"
			req.Inputs = []project.Input{{Name: "candidate", Artifact: candidate}}
		}
		if _, err := snapshots.Materialize(ctx, req); err == nil {
			t.Errorf("ordinary materialization trusted rejected candidate: asInput=%v", asInput)
		}
	}
}

func TestRecoveryExactCandidateReplayPreservesLateDiskAndContinuousHeads(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	first := runCopyAttempt(t, c, recoveryCopySpec(t, c, p, ws, "cas-first"))
	if err := os.WriteFile(filepath.Join(ws.Path, "result"), []byte("accepted candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), first.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	first, _ = c.attempts.Get(t.Context(), first.ID)
	completion, _, err := c.completion(t.Context(), first, Result{Text: "done"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Binding == nil || completion.Result.Artifact == "" {
		t.Fatal("fixture has no prepared candidate")
	}
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_result_name BEFORE INSERT ON names WHEN NEW.name LIKE 'steve/%' BEGIN SELECT RAISE(ABORT,'controlled result name refused'); END`)
	_, cause := c.attempts.FinishCompletion(t.Context(), first.ID, "fixture", completion)
	if cause == nil || !strings.Contains(cause.Error(), "controlled result name refused") {
		t.Fatalf("wrong refusal: %v", cause)
	}
	if err := c.attempts.RejectCompletion(t.Context(), first.ID, "fixture", completion, cause); err == nil {
		t.Fatal("rejection hid original refusal")
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Head.Version != 1 || episode.Producer == nil {
		t.Fatalf("name refusal partially advanced recovery: %+v %v", episode, err)
	}
	if _, found, err := c.artifacts.Resolve(t.Context(), completion.Binding.Name); err != nil || found {
		t.Fatalf("name refusal partially installed name: %v %v", found, err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "result"), []byte("late uncaptured disk bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	queued := recoveryCopySpec(t, c, p, ws, "cas-next")
	if _, err := c.attempts.Open(t.Context(), queued); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("unpublished producer allowed a new input: %v", err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_result_name"); return err }); err != nil {
		t.Fatal(err)
	}
	fresh := attempt.New(ledgerOf(t, c))
	replay, err := fresh.PublishRecoveryHead(t.Context(), episode.ID, c.artifacts.RecoveryOutputTx)
	if err != nil || replay.Head.Version != 2 || replay.Head.Artifact != completion.Result.Artifact || replay.Producer != nil || len(replay.Head.Sources) != 1 {
		t.Fatalf("reopen did not replay exact candidate: %+v %v", replay, err)
	}
	next := runCopyAttempt(t, c, queued)
	if next.Base != completion.Result.Artifact || next.WorkspaceRecovery.HeadVersion != 2 {
		t.Fatalf("next writer bound stale head: %+v", next.Spec)
	}
	if raw, err := os.ReadFile(filepath.Join(ws.Path, "result")); err != nil || string(raw) != "late uncaptured disk bytes\n" {
		t.Fatalf("metadata replay re-cut or reset disk: %q %v", raw, err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), next.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	next, _ = c.attempts.Get(t.Context(), next.ID)
	second, _, err := c.completion(t.Context(), next, Result{Text: "next"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FinishCompletion(t.Context(), next.ID, "fixture", second); err != nil {
		t.Fatal(err)
	}
	final, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || final.Head.Version != 3 || len(final.Head.Sources) != 2 {
		t.Fatalf("continuous B/Head1/Head2 lost authority: %+v %v", final, err)
	}
	if _, err := c.tasks.Finish(first.TaskID, task.OutcomeError, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.DeleteChannel(t.Context(), "console:cas-first", attempt.CheckTaskDeletionTx); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("prior producer authority was deleted: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "original")); err != nil || string(raw) != "named base\n" {
		t.Fatalf("replay touched original directory: %q %v", raw, err)
	}
	t.Logf("controlled CAS rejection/reopen replay: B=%s Head1=%s Head2=%s producers=%d", episode.Head.Artifact, replay.Head.Artifact, final.Head.Artifact, len(final.Head.Sources))
}

func TestRecoveryTransferKeepsTheHoldAfterAllSourceRetirement(t *testing.T) {
	c, p, source, _ := sharedCopy(t)
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: source.Session, Harness: source.Harness, ProcessStopped: true, State: nodewire.SessionClosed, Binding: nodewire.SessionBinding{ProjectID: source.Project, SessionID: attempt.RetainedSessionID("console:recovery", source.TaskID, source.Agent), TaskID: source.TaskID, AttemptID: source.ID, NodeID: source.Node, TaskEpoch: source.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(source)}}}
	var err error
	source, err = c.attempts.ConfirmTaskStopped(t.Context(), source.ID, "fixture", proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.MarkStopProjected(t.Context(), source.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.CompleteAbandonDelivery(t.Context(), source, func(ledger.Reader, attempt.Record) (attempt.AbandonDelivery, error) {
		return attempt.AbandonDeliveryNotRequired, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Release's owner transaction must reject before modifying project-owner state.
	c.projects.SetHubID("source-hub")
	_, err = c.projects.Release(t.Context(), p.ID, "target-hub", "controlled-transfer", "verified stopped", attempt.ReleaseProjectGuard)
	if !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("transfer released a held recovery: %v", err)
	}
	if _, found, err := c.projects.Ownership(t.Context(), p.ID); err != nil || found {
		t.Fatalf("refused transfer installed owner state: %v %v", found, err)
	}
	if _, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryHoldAndAdmittedLandingHaveBothSafeOrders(t *testing.T) {
	for _, afterApplying := range []bool{false, true} {
		t.Run(map[bool]string{false: "hold wins before applying", true: "applying wins before hold"}[afterApplying], func(t *testing.T) {
			gate := &recoverySnapshotGate{LocalNodes: artifact.LocalNodes{Dir: t.TempDir()}, entered: make(chan struct{}), release: make(chan struct{})}
			if afterApplying {
				gate.blockOp = ops.Apply
			}
			c, p, source, base := recoveryCanonicalWriterFixture(t, gate, false)
			ws, err := c.artifacts.Materialize(t.Context(), project.Request{Project: p.ID, Node: p.Home.Node, Isolated: true, Base: base, Owner: "controlled-incoming"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws.Path, "from-child"), []byte("child change\n"), 0600); err != nil {
				t.Fatal(err)
			}
			incoming, changed, err := c.artifacts.Publish(t.Context(), ws, base, "fixture", "incoming")
			if err != nil || !changed {
				t.Fatalf("prepare incoming: %v %v", changed, err)
			}
			held, ok := artifact.CanonicalLease(source.Leases, p.ID)
			if !ok {
				t.Fatal("missing borrowed canonical lease")
			}
			gate.enabled.Store(true)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			done := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := c.artifacts.LandUnder(ctx, p, incoming.ID, "fixture", held)
				done <- err
			}()
			defer func() {
				select {
				case <-gate.release:
				default:
					close(gate.release)
				}
				wg.Wait()
			}()
			select {
			case <-gate.entered:
			case err := <-done:
				t.Fatalf("landing never reached controlled I/O: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := c.attempts.MarkUnsettled(ctx, source.ID, "fixture", errors.New("writer unreachable"), nil); err != nil {
				t.Fatal(err)
			}
			if err := NewForceStopControl(c).ForceStopAttempt(ctx, source.ID, "owner", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := c.attempts.RecordForceStopResult(ctx, source.ID, 1, true, "stop_unproven"); err != nil {
				t.Fatal(err)
			}
			before, _ := c.tasks.Get(source.TaskID)
			abandoned, abandonErr := NewAbandonControl(c).AbandonAttempt(ctx, source.ID, "owner", 1)
			if afterApplying {
				if abandonErr == nil || !strings.Contains(abandonErr.Error(), "canonical landing must settle") {
					t.Fatalf("hold was committed while admitted WAL can write: %v", abandonErr)
				}
				after, _ := c.tasks.Get(source.TaskID)
				if !sameAbandonJSON(before, after) {
					t.Fatal("refused AB changed cached task accounting")
				}
				if _, found, err := c.attempts.RecoveryForProject(ctx, p.ID); err != nil || found {
					t.Fatalf("refused AB left an episode: %v %v", found, err)
				}
			} else if abandonErr != nil || abandoned.Abandoned.WorkspaceRecoveryID == "" {
				t.Fatalf("early hold did not commit: %v", abandonErr)
			}
			close(gate.release)
			landErr := <-done
			if afterApplying {
				if landErr != nil {
					t.Fatalf("already admitted WAL did not finish before hold: %v", landErr)
				}
				if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "from-child")); err != nil || string(raw) != "child change\n" {
					t.Fatalf("WAL not applied: %q %v", raw, err)
				}
				if _, err := NewAbandonControl(c).AbandonAttempt(ctx, source.ID, "owner", 1); err != nil {
					t.Fatalf("hold still refused after WAL settled: %v", err)
				}
			} else {
				if !errors.Is(landErr, attempt.ErrWorkspaceRecovery) {
					t.Fatalf("pre-applying landing bypassed hold: %v", landErr)
				}
				if gate.applyCalls.Load() != 0 {
					t.Fatalf("hold allowed original-directory Apply: %d", gate.applyCalls.Load())
				}
				if _, err := os.Stat(filepath.Join(p.Home.Path, "from-child")); !os.IsNotExist(err) {
					t.Fatalf("blocked landing wrote original directory: %v", err)
				}
			}
			t.Logf("applying-first=%v AB-error=%v landing-error=%v apply-calls=%d", afterApplying, abandonErr, landErr, gate.applyCalls.Load())
		})
	}
}

func TestRecoveryMissingAcceptedProducerChainRefusesDeletion(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	first := runCopyAttempt(t, c, recoveryCopySpec(t, c, p, ws, "corrupt-chain-producer"))
	if err := os.WriteFile(filepath.Join(ws.Path, "produced"), []byte("must retain authority\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), first.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	first, _ = c.attempts.Get(t.Context(), first.ID)
	completion, _, err := c.completion(t.Context(), first, Result{Text: "done"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FinishCompletion(t.Context(), first.ID, "fixture", completion); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.Finish(first.TaskID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Head.Version != 2 || len(episode.Head.Sources) != 1 {
		t.Fatalf("fixture has no accepted producer: %+v %v", episode, err)
	}
	if _, err := c.tasks.DeleteChannel(t.Context(), "console:corrupt-chain-producer", attempt.CheckTaskDeletionTx); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("unmodified guard did not retain producer: %v", err)
	} else {
		t.Logf("uncorrupted owner refused producer deletion: %v", err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE operations SET data=json_remove(data,'$.head.sources') WHERE id=?`, episode.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Valid JSON with an impossible version/authority chain is corruption, not an empty history.
	decoded, decodeErr := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	deleted, deleteErr := c.tasks.DeleteChannel(t.Context(), "console:corrupt-chain-producer", attempt.CheckTaskDeletionTx)
	if deleteErr == nil {
		t.Fatalf("corrupt head version=%d with no producing chain decoded with err=%v and deleted accepted authority: tasks=%v producer=%s", decoded.Head.Version, decodeErr, deleted, first.TaskID)
	}
}

func TestRecoveryAcceptedChainRejectsEveryMissingOrChangedAuthority(t *testing.T) {
	for _, which := range []string{"middle", "base", "version", "head", "repeat", "token", "attempt", "may-write", "evidence"} {
		t.Run(which, func(t *testing.T) {
			c, p, source, ws := sharedCopy(t)
			for index := range 2 {
				r := runCopyAttempt(t, c, recoveryCopySpec(t, c, p, ws, fmt.Sprintf("chain-%d", index)))
				if err := os.WriteFile(filepath.Join(ws.Path, "chain"), []byte(fmt.Sprintf("output %d", index)), 0600); err != nil {
					t.Fatal(err)
				}
				if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
					t.Fatal(err)
				}
				r, _ = c.attempts.Get(t.Context(), r.ID)
				completion, _, err := c.completion(t.Context(), r, Result{Text: "done"}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.attempts.FinishCompletion(t.Context(), r.ID, "fixture", completion); err != nil {
					t.Fatal(err)
				}
			}
			episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "middle":
				episode.Head.Sources = episode.Head.Sources[1:]
			case "base":
				episode.Head.Sources[1].Base = episode.Baseline.Artifact
			case "version":
				episode.Head.Sources[1].HeadVersion = 1
			case "head":
				episode.Head.Artifact = episode.Baseline.Artifact
			case "repeat":
				episode.Head.Sources[1].Attempt = episode.Head.Sources[0].Attempt
			case "token":
				episode.Head.Sources[0].Execution.Epoch++
			case "attempt":
				episode.Head.Sources[0].Attempt = source.ID
			case "may-write":
				no := false
				episode.Head.Sources[0].NativeMayWrite = &no
			case "evidence":
				episode.Head.Sources[0].Evidence = ""
			}
			raw, err := json.Marshal(episode)
			if err != nil {
				t.Fatal(err)
			}
			if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec("UPDATE operations SET data=? WHERE id=?", string(raw), episode.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID); err == nil {
				t.Fatal("changed accepted authority decoded as trusted")
			}
			if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
				return attempt.CheckTaskDeletionTx(tx, []string{episode.Head.Sources[0].Execution.TaskID})
			}); err == nil {
				t.Fatal("changed authority allowed deletion")
			}
			if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { return attempt.ReleaseProjectGuard(tx, p.ID) }); err == nil {
				t.Fatal("changed authority allowed transfer")
			}
		})
	}
}

func TestRecoveryAcceptedHistoricalEvidenceNeverRestoresARevokedToken(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	r := runCopyAttempt(t, c, recoveryCopySpec(t, c, p, ws, "accepted-revoked"))
	if err := os.WriteFile(filepath.Join(ws.Path, "result"), []byte("accepted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	r, _ = c.attempts.Get(t.Context(), r.ID)
	completion, _, err := c.completion(t.Context(), r, Result{Text: "done"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FinishCompletion(t.Context(), r.ID, "fixture", completion); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.SetAside(r.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID); err != nil {
		t.Fatalf("revocation destroyed historical facts: %v", err)
	}
	if _, _, err := c.artifacts.PublishRecovery(t.Context(), ws, r.Base, r.ID, "revoked retry"); err == nil {
		t.Fatal("historical evidence granted a revoked publisher")
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { return task.CheckExecutionTx(tx, r.Execution) }); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("history reauthorized revoked token: %v", err)
	}
}

func TestRecoveryLateSameSHASnapshotPreservesItsEarlierAcceptedAuthority(t *testing.T) {
	gate := &recoverySnapshotGate{LocalNodes: artifact.LocalNodes{Dir: t.TempDir()}, entered: make(chan struct{}), release: make(chan struct{})}
	c, p, source, base := recoveryCanonicalWriterFixture(t, gate, false)
	before, found, err := c.artifacts.Manifest(t.Context(), base)
	if err != nil || !found {
		t.Fatal("fixture lacks its earlier accepted artifact")
	}
	held, _ := artifact.CanonicalLease(source.Leases, p.ID)
	gate.enabled.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var joined sync.WaitGroup
	joined.Add(1)
	go func() {
		defer joined.Done()
		_, _, err := c.artifacts.SnapshotCanonicalUnder(ctx, p, held, base, source.ID, "same accepted bytes")
		done <- err
	}()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
		joined.Wait()
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.attempts.MarkUnsettled(ctx, source.ID, "fixture", errors.New("unreachable"), nil); err != nil {
		t.Fatal(err)
	}
	if err := NewForceStopControl(c).ForceStopAttempt(ctx, source.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.RecordForceStopResult(ctx, source.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAbandonControl(c).AbandonAttempt(ctx, source.ID, "owner", 1); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-done; !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("late same-SHA operation was not refused: %v", err)
	}
	after, found, err := c.artifacts.Manifest(ctx, base)
	if err != nil || !found || !sameAbandonJSON(before, after) {
		t.Fatal("refusing a late cut revoked or changed the earlier same-SHA artifact")
	}
	if err := ledgerOf(t, c).Read(ctx, func(tx *ledger.ReadTx) error {
		_, err := c.artifacts.RecoveryOutputTx(tx, p.ID, base, base)
		return err
	}); err != nil {
		t.Fatal("earlier accepted base lost its independent authority", err)
	}
	if _, err := c.artifacts.Materialize(ctx, project.Request{Project: p.ID, Isolated: true, Base: base, Owner: "accepted-earlier"}); err != nil {
		t.Fatal("refusing a late candidate removed a legitimate explicit base", err)
	}
}

func TestRecoverySameArtifactKeepsDifferentProducerAuthoritiesDistinct(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	var accepted string
	for index := range 2 {
		r := runCopyAttempt(t, c, recoveryCopySpec(t, c, p, ws, fmt.Sprintf("same-artifact-%d", index)))
		if index == 0 {
			if err := os.WriteFile(filepath.Join(ws.Path, "result"), []byte("shared bytes"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
			t.Fatal(err)
		}
		r, _ = c.attempts.Get(t.Context(), r.ID)
		completion, _, err := c.completion(t.Context(), r, Result{Text: "done"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			accepted = completion.Result.Artifact
		} else {
			// A nonempty exact candidate may reuse an already accepted SHA.
			// Its result-name decision still belongs to this new producer.
			name := "steve/" + r.TaskID + "/turn/" + r.TurnID
			completion.Result.Artifact = accepted
			completion.Result.RecoveryOutput = &attempt.RecoveryOutput{Name: name}
			completion.Binding = &attempt.NameBinding{Name: name}
		}
		if _, err := c.attempts.FinishCompletion(t.Context(), r.ID, "fixture", completion); err != nil {
			t.Fatal(err)
		}
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || len(episode.Head.Sources) != 2 || episode.Head.Version != 3 {
		t.Fatalf("content reuse lost distinct sources: %+v %v", episode, err)
	}
	a, b := episode.Head.Sources[0], episode.Head.Sources[1]
	if a.Artifact != b.Artifact || a.Evidence != b.Evidence || a.Attempt == b.Attempt || a.Execution == b.Execution || a.HeadVersion == b.HeadVersion {
		t.Fatal("content evidence was confused with producing authority")
	}
}
