package artifact

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestPendingLandingDrainersDoNotCreateCompetingRecords(t *testing.T) {
	canonical := t.TempDir()
	write(t, canonical, "file", "before")
	s, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, err := s.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: "child"})
	if err != nil {
		t.Fatal(err)
	}
	base := s.canonicalRef(t.Context(), p.ID)
	write(t, ws.Path, "file", "accepted")
	result, _, err := s.Publish(t.Context(), ws, base, "child", "result")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Defer(t.Context(), p.ID, result.ID, "child"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var once atomic.Bool
	s.now = func() time.Time {
		if once.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return time.Now()
	}
	first := make(chan error, 1)
	go func() { _, err := s.LandPending(t.Context(), p); first <- err }()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("first drainer never reached landing: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first drainer did not enter landing")
	}
	_, secondErr := s.LandPending(t.Context(), p)
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(secondErr, ledger.ErrHeld) {
		t.Fatalf("second drainer created competing work: %v", secondErr)
	}
	landings, err := s.Landings(t.Context(), p.ID)
	if err != nil || len(landings) != 1 || landings[0].State != LandCommitted || read(t, canonical, "file") != "accepted" {
		t.Fatalf("landing was duplicated or lost: %+v %v", landings, err)
	}
	pending, err := s.ledger.Bindings(t.Context(), pendingKind)
	if err != nil || len(pending) != 0 {
		t.Fatal("completed queue item survived")
	}
}

func TestCompletionKeepsRealLandingConflictsAndAcceptsSupersededLockRefusal(t *testing.T) {
	for _, scenario := range []string{"same-result", "no-commit", "different-artifact", "different-target", "different-attempt", "different-epoch", "write-lease", "snapshot-taken", "paths", "wal", "unfinished", "apply-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
			refused := Landing{ID: "refused", Project: "p", Artifact: "result", Target: project.Home{Path: "/canonical"}, Source: &Source{AttemptID: "child-attempt", Execution: &task.ExecutionToken{TaskID: "child", Epoch: 1}}, State: LandMergeConflicted, EndedAt: time.Now()}
			committed := refused
			committed.ID, committed.State = "committed", LandCommitted
			switch scenario {
			case "different-artifact":
				committed.Artifact = "different"
			case "different-target":
				committed.Target.Path = "/elsewhere"
			case "different-attempt":
				committed.Source = &Source{AttemptID: "different", Execution: refused.Source.Execution}
			case "different-epoch":
				committed.Source = &Source{AttemptID: "child-attempt", Execution: &task.ExecutionToken{TaskID: "child", Epoch: 2}}
			case "write-lease":
				refused.Lease = &ledger.Lease{Key: "canonical:p"}
			case "snapshot-taken":
				refused.Now = "snapshot"
			case "paths":
				refused.Paths = []string{"file"}
			case "wal":
				refused.Round = 1
			case "unfinished":
				refused.EndedAt = time.Time{}
			case "apply-conflict":
				refused.State = LandApplyConflicted
			}
			for _, land := range []Landing{refused, committed} {
				if scenario == "no-commit" && land.ID == committed.ID {
					continue
				}
				if _, err := s.ledger.Begin(t.Context(), land.ID, landKind, land.State, "test", land); err != nil {
					t.Fatal(err)
				}
			}
			err := s.ledger.Update(t.Context(), func(tx *ledger.Tx) error {
				return CheckTaskLandingsTx(tx, map[string]bool{"child": true})
			})
			if (err == nil) != (scenario == "same-result") {
				t.Fatalf("completion accepted unresolved landing or rejected its committed result: %v", err)
			}
			op, found, err := s.ledger.Operation(t.Context(), refused.ID)
			if err != nil || !found || op.State != refused.State {
				t.Fatal("completion guard rewrote historical landing evidence")
			}
		})
	}
}

func TestCompletionBlocksQueuedResultsBeforeLandingHasStarted(t *testing.T) {
	s, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	pending := Pending{Project: "p", Artifact: "not-landed", Source: &Source{AttemptID: "child-attempt", Execution: &task.ExecutionToken{TaskID: "child", Epoch: 1}}}
	if err := s.ledger.PutBinding(t.Context(), pendingKind, "p/not-landed", pending); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"child", "unrelated"} {
		err := s.ledger.Update(t.Context(), func(tx *ledger.Tx) error {
			return CheckTaskLandingsTx(tx, map[string]bool{id: true})
		})
		if errors.Is(err, task.ErrCompleteDelivery) != (id == "child") || (id == "unrelated" && err != nil) {
			t.Fatalf("queued result completion for %s: %v", id, err)
		}
	}
}
