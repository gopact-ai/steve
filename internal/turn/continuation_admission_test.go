package turn

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type openingAttempts struct {
	lifecycle.Attempts
	open func(context.Context, attempt.Spec) (attempt.Record, error)
}

func (a openingAttempts) Open(ctx context.Context, spec attempt.Spec) (attempt.Record, error) {
	return a.open(ctx, spec)
}

func TestContinuationRetriesOnlyDefinitivelyUnopenedAttempts(t *testing.T) {
	for _, refusal := range []error{attempt.Busy{Resource: "canonical:p"}, attempt.NoSlot{Endpoint: "worker", Slots: 1}} {
		calls, notifications, id := 0, 0, ""
		opener := waitingAttempts{passes: continuationPasses, Attempts: openingAttempts{open: func(ctx context.Context, spec attempt.Spec) (attempt.Record, error) {
			calls++
			if calls == 1 {
				id = spec.ID
				return attempt.Record{}, refusal
			}
			if id == "" || spec.ID != id || spec.TurnID != "original-turn" {
				t.Fatal("retry lost original execution identity")
			}
			return attempt.Record{Spec: spec, State: attempt.Leased}, nil
		}}, waiting: func() { notifications++ }}
		if _, err := opener.Open(t.Context(), attempt.Spec{TurnID: "original-turn"}); err != nil || calls != 2 || notifications != 1 {
			t.Fatalf("retry: calls=%d notices=%d err=%v", calls, notifications, err)
		}
	}
	for _, scenario := range []string{"unknown outcome", "record exists", "cancelled"} {
		calls := 0
		opener := waitingAttempts{passes: continuationPasses, Attempts: openingAttempts{open: func(context.Context, attempt.Spec) (attempt.Record, error) {
			calls++
			switch scenario {
			case "record exists":
				return attempt.Record{Spec: attempt.Spec{ID: "accepted"}}, attempt.Busy{}
			case "cancelled":
				return attempt.Record{}, context.Canceled
			default:
				return attempt.Record{}, errors.New("connection lost after request")
			}
		}}}
		if _, err := opener.Open(t.Context(), attempt.Spec{}); err == nil || calls != 1 {
			t.Fatalf("replayed %s: calls=%d err=%v", scenario, calls, err)
		}
	}
}

func TestParentContinuationWaitsForLandingWithoutDuplicatingPromptOrBudget(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(map[bool]string{false: "landing finishes", true: "user pauses"}[pause], func(t *testing.T) {
			runner := &fakeRunner{reply: "child result processed"}
			c, _ := completionCoordinator(t, runner)
			root, err := c.tasks.Create(task.Task{Channel: "chat", Member: "codex", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.tasks.Advance(root.ID, task.StateRunning); err != nil {
				t.Fatal(err)
			}
			release, err := c.attempts.Hold(t.Context(), "", "canonical:p", "sibling-landing")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			finished := make(chan error, 1)
			go func() {
				_, err := c.Handle(t.Context(), Request{ConversationID: "chat", Input: "retained child result", MessageID: "delivery-original", ExpectedTask: root.ID})
				finished <- err
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				tracked, _ := c.tasks.Get(root.ID)
				if len(tracked.Attempts) == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("continuation was not reserved")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case err := <-finished:
				t.Fatalf("landing contention lost the continuation: %v", err)
			case <-time.After(350 * time.Millisecond):
			}
			if len(runner.seen()) != 0 {
				t.Fatal("prompt ran while sibling held the workspace")
			}
			if pause {
				if _, err := handle(c, t.Context(), "/tasks pause "+root.ID); err != nil {
					t.Fatal(err)
				}
			}
			release()
			select {
			case err := <-finished:
				if pause && !errors.Is(err, context.Canceled) {
					t.Fatalf("pause: %v", err)
				}
				if !pause && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("continuation did not settle")
			}
			tracked, _ := c.tasks.Get(root.ID)
			if tracked.Budget.Turns != 1 || len(tracked.Attempts) != 1 || tracked.Attempts[0].Open() {
				t.Fatal("retries duplicated accounting or left an open execution")
			}
			want := 1
			if pause {
				want = 0
				if tracked.State != task.StatePaused {
					t.Fatal("pause was undone")
				}
			}
			if len(runner.seen()) != want {
				t.Fatalf("prompts=%d want=%d", len(runner.seen()), want)
			}
		})
	}
}

func TestOrdinaryPromptExplainsNonAgentWorkspaceWriter(t *testing.T) {
	c, _ := completionCoordinator(t, &fakeRunner{reply: "must not run"})
	release, err := c.attempts.Hold(t.Context(), "", "canonical:p", "landing-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = handle(c, t.Context(), "new work")
	if err == nil || !strings.Contains(err.Error(), "receiving results") || strings.Contains(err.Error(), "(task #)") {
		t.Fatalf("unhelpful writer explanation: %v", err)
	}
}

// A snapshot cut for new work holds the canonical lock for as long as the
// cut takes and gives it back by itself: an in-place prompt waits it out
// instead of being told the project is being written.
func TestOrdinaryPromptWaitsOutACanonicalSnapshot(t *testing.T) {
	c, _ := completionCoordinator(t, &fakeRunner{reply: "after the snapshot"})
	release, err := c.attempts.Hold(t.Context(), "", "canonical:p", artifact.SnapshotHolder("plan"))
	if err != nil {
		t.Fatal(err)
	}
	// The snapshot gives the lock back once the prompt says it is waiting,
	// so the prompt meets the lock however long it takes to reach it. The
	// timer only lets a prompt that never says so go on.
	var once sync.Once
	giveBack := func() { once.Do(release) }
	timer := time.AfterFunc(5*time.Second, giveBack)
	defer timer.Stop()
	var mu sync.Mutex
	var stages []view.Stage
	result, err := c.Handle(t.Context(), Request{ConversationID: "chat", Input: "new work", OnStage: func(s view.Stage) {
		mu.Lock()
		stages = append(stages, s)
		mu.Unlock()
		if s == view.StageAwaitSnapshot {
			go giveBack()
		}
	}})
	if err != nil {
		t.Fatalf("a prompt behind a snapshot was refused: %v", err)
	}
	if !strings.Contains(result.Text, "after the snapshot") {
		t.Fatalf("result = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := slices.Index(stages, view.StageAwaitSnapshot); n < 0 || slices.Index(stages[n+1:], view.StageAwaitSnapshot) >= 0 {
		t.Fatalf("stages = %v, want the wait for the snapshot said once", stages)
	}
}

// The wait for a snapshot is bounded: a snapshot holding the lock past it
// is a busy project, said once when the wait began.
func TestSnapshotWaitIsBounded(t *testing.T) {
	opens, said := 0, 0
	opener := waitingAttempts{passes: snapshotPasses, limit: 400 * time.Millisecond, waiting: func() { said++ },
		Attempts: openingAttempts{open: func(context.Context, attempt.Spec) (attempt.Record, error) {
			opens++
			return attempt.Record{}, attempt.Busy{Resource: "canonical:p", Holder: artifact.SnapshotHolder("plan")}
		}}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	_, err := opener.Open(ctx, attempt.Spec{})
	var busy attempt.Busy
	if !errors.As(err, &busy) || ctx.Err() != nil {
		t.Fatalf("open after %s = %v, want the snapshot's busy once the wait ran out", time.Since(started), err)
	}
	if opens < 2 || said != 1 {
		t.Fatalf("opens=%d said=%d, want retries said once", opens, said)
	}
}
