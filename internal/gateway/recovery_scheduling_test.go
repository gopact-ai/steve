package gateway

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type concurrentResumeProbe struct {
	turntest.IdleCoordinator
	tasks   *task.Store
	entered chan string
	release chan struct{}
}

func (p *concurrentResumeProbe) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	_, err := p.tasks.BeginTurn(req.ExpectedTask, "worker", "", task.TurnInput{Address: channel.Address{Channel: "feishu", Conversation: req.ConversationID, Message: req.MessageID}, Continuation: true, ResumeAdmission: req.ResumeAdmission, TurnID: req.MessageID})
	if err != nil {
		return turn.Result{}, err
	}
	id := "attempt-" + req.ExpectedTask
	if req.OnTurnReady != nil {
		req.OnTurnReady(req.ExpectedTask, id)
	}
	p.entered <- req.ConversationID
	if req.ConversationID == "slow" {
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}
	_, err = p.tasks.Finish(req.ExpectedTask, task.OutcomeOK, task.Tokens{}, 0)
	return turn.Result{Text: "complete original result", Attempt: id}, err
}
func TestGatewayIndependentManualResumeDoesNotWaitForOtherNativeTurn(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	p := &concurrentResumeProbe{tasks: tasks, entered: make(chan string, 2), release: make(chan struct{})}
	g := New(p)
	g.BindChannel(&recoveryChannel{})
	admissions := []task.ResumeAdmission{}
	for _, conversation := range []string{"slow", "independent"} {
		row, err := tasks.Create(task.Task{Transport: "feishu", Channel: conversation, Member: "worker", Requester: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tasks.SetAside(row.ID, task.StatePaused); err != nil {
			t.Fatal(err)
		}
		row, _ = tasks.Get(row.ID)
		a := task.ResumeAdmission{ID: "resume-" + conversation, TaskID: row.ID, Epoch: row.ExecutionEpoch + 1}
		if err = g.QueueTaskResume(t.Context(), book, a.ID, Revival{TaskID: row.ID, ConversationID: conversation, Member: "worker", MessageID: "anchor-" + conversation, Requester: "owner", Manual: true}, a); err != nil {
			t.Fatal(err)
		}
		if _, err = tasks.Resume(row.ID, row.ExecutionEpoch, row.State, a); err != nil {
			t.Fatal(err)
		}
		admissions = append(admissions, a)
	}
	ctx, cancel := context.WithCancel(t.Context())
	type completion struct {
		task string
		err  error
	}
	done := make(chan completion, 2)
	wake := func(a task.ResumeAdmission) {
		done <- completion{task: a.TaskID, err: g.DispatchResume(ctx, book, a, nil, func(string, string) error { return nil })}
	}
	completed := 0
	started := 0
	released := false
	defer func() {
		cancel()
		if !released {
			close(p.release)
		}
		for completed < started {
			select {
			case result := <-done:
				completed++
				if result.err != nil && !errors.Is(result.err, context.Canceled) {
					t.Errorf("resume %s: %v", result.task, result.err)
				}
			case <-time.After(10 * time.Second):
				t.Error("resume workers did not join after cancellation")
				return
			}
		}
	}()
	awaitEntry := func(want string) {
		t.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case got := <-p.entered:
				if got != want {
					t.Fatalf("wanted %s Handle, got %s", want, got)
				}
				return
			case result := <-done:
				completed++
				if result.err != nil {
					t.Fatalf("resume %s returned before %s Handle: %v", result.task, want, result.err)
				}
				// A fast independent Handle can finish after publishing entry
				// but before this goroutine is scheduled to receive it.
			case <-deadline.C:
				g.mu.Lock()
				claims, conversations, slots := len(g.durableRunning), len(g.serving), len(g.slots)
				g.mu.Unlock()
				t.Fatalf("%s Handle never entered while first release stayed closed: claims=%d conversations=%d slots=%d completed=%d/%d", want, claims, conversations, slots, completed, started)
			}
		}
	}
	started++
	go wake(admissions[0])
	awaitEntry("slow")
	started++
	go wake(admissions[1])
	awaitEntry("independent")
	// The assertion is causal, not a 200ms performance requirement: the
	// independent Handle must enter BEFORE the first native turn is released.
	select {
	case <-p.release:
		t.Fatal("test released the slow native turn before independent admission")
	default:
	}
	close(p.release)
	released = true
}

type blockedRecoveryProbe struct {
	turntest.IdleCoordinator
	calls, resumes atomic.Int32
	entered        chan struct{}
	release        chan struct{}
}

func (p *blockedRecoveryProbe) wait(ctx context.Context) error {
	p.entered <- struct{}{}
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *blockedRecoveryProbe) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	p.calls.Add(1)
	if req.OnTurnReady != nil {
		req.OnTurnReady(req.ExpectedTask, "original-attempt")
	}
	if err := p.wait(ctx); err != nil {
		return turn.Result{}, err
	}
	return turn.Result{Text: "complete original result", Attempt: "original-attempt"}, nil
}

func (p *blockedRecoveryProbe) ResumeRetainedChat(ctx context.Context, id string, _ turn.Request) (turn.Result, error) {
	if err := ctx.Err(); err != nil {
		return turn.Result{}, err
	}
	p.resumes.Add(1)
	if err := p.wait(ctx); err != nil {
		return turn.Result{}, err
	}
	return turn.Result{Text: "complete original result", Attempt: id}, nil
}

func TestGatewaySameInputJoinsNativeAndUnknownRetainedObserver(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "unknown-dispatch"}[unknown], func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			p := &blockedRecoveryProbe{entered: make(chan struct{}, 3), release: make(chan struct{})}
			g := New(p)
			ch := &recoveryChannel{}
			g.BindChannel(ch)
			key := "original"
			if err := g.QueueRecovery(t.Context(), book, key, revivalFixture(), ""); err != nil {
				t.Fatal(err)
			}
			if unknown {
				if err := book.RecordCommand(t.Context(), key+"/notice", "gateway-recovery-notice", "owner", []byte(`"notice-receipt"`)); err != nil {
					t.Fatal(err)
				}
				req := turn.Request{ExpectedTask: "parent", ConversationID: "conversation", MessageID: "notice-receipt"}
				if err := rememberRecoveryAttempt(t.Context(), book, key, "owner", req, "parent", "original-attempt"); err != nil {
					t.Fatal(err)
				}
				if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at)
					VALUES('original/dispatch','gateway-recovery-dispatch','owner','2026-09-19T00:00:00Z')`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 2)
			go func() { done <- g.recoverQueuedFixture(ctx, book, p, func(string, string) error { return nil }) }()
			select {
			case <-p.entered:
			case <-time.After(time.Second):
				cancel()
				t.Fatal("original observer never started")
			}
			go func() { done <- g.recoverQueuedFixture(ctx, book, p, func(string, string) error { return nil }) }()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, channel.ErrDeliveryQueued) {
					t.Error(err)
				}
			case <-time.After(time.Second):
				cancel()
				<-done
				<-done
				t.Fatal("duplicate wake waited for the original native/retained observer")
			}
			if p.calls.Load()+p.resumes.Load() != 1 {
				t.Error("same input concurrently entered two native/retained observers")
			}
			cancel()
			<-done
			// Cancellation must release only the in-process owner. The unknown
			// dispatch remains fenced; retry observes its original attempt.
			close(p.release)
			if err := g.recoverQueuedFixture(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			expectedCalls := int32(1)
			if unknown {
				expectedCalls = 0
			}
			if p.calls.Load() != expectedCalls || p.resumes.Load() != 2-expectedCalls || ch.results.Load() != 1 {
				t.Fatalf("cancellation lost ownership or replayed input: calls=%d resumes=%d replies=%d", p.calls.Load(), p.resumes.Load(), ch.results.Load())
			}
		})
	}
}

type recoveryTestWorkers struct{ sync.WaitGroup }

func (w *recoveryTestWorkers) Go(run func()) bool {
	w.WaitGroup.Go(run)
	return true
}

func TestGatewayRuntimeRecoveryUsesBoundedSlotsAndCancellation(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &blockedRecoveryProbe{entered: make(chan struct{}, 10), release: make(chan struct{})}
	g := New(p)
	g.slots = make(chan struct{}, 2)
	g.BindChannel(&recoveryChannel{})
	for _, key := range []string{"one", "two", "three", "four", "five"} {
		input := revivalFixture()
		input.ConversationID = key
		if err := g.QueueRecovery(t.Context(), book, key, input, ""); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	var workers recoveryTestWorkers
	defer func() { cancel(); workers.Wait() }()
	for range 4 {
		if err := g.ReconcileQueued(ctx, book, p, func(string, string) error { return nil }, &workers); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case <-p.entered:
		case <-time.After(time.Second):
			t.Fatal("available recovery slots were not used")
		}
	}
	if p.calls.Load() != 2 || p.resumes.Load() != 0 {
		t.Fatalf("pending inputs created unbounded observers: calls=%d resumes=%d", p.calls.Load(), p.resumes.Load())
	}
	cancel()
	workers.Wait()
	g.mu.Lock()
	inFlight := len(g.durableRunning)
	g.mu.Unlock()
	if inFlight != 0 || len(g.slots) != 0 {
		t.Fatalf("cancelled workers leaked claims/slots: claims=%d slots=%d", inFlight, len(g.slots))
	}
}

type heldRecoveryWorkers struct{ jobs []func() }

func (w *heldRecoveryWorkers) Go(run func()) bool {
	w.jobs = append(w.jobs, run)
	return true
}

type closedRecoveryWorkers struct{}

func (closedRecoveryWorkers) Go(func()) bool { return false }

func TestGatewayClosedWorkerAdmissionReleasesClaimAndSlot(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &recoveryProbe{}
	g := New(p)
	g.BindChannel(&recoveryChannel{})
	if err := g.QueueRecovery(t.Context(), book, "original", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	if err := g.ReconcileQueued(t.Context(), book, p, func(string, string) error { return nil }, closedRecoveryWorkers{}); err == nil {
		t.Fatal("closed lifetime was not reported")
	}
	if len(g.durableRunning) != 0 || len(g.slots) != 0 || p.calls.Load() != 0 {
		t.Fatal("rejected worker leaked ownership or dispatched work")
	}
	if err := g.recoverQueuedFixture(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayRuntimeRecoveryDoesNotStarveBehindUnknownReceipts(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p, ch := &recoveryProbe{}, &recoveryChannel{noticeErr: channel.ErrOutcomeUnknown}
	g := New(p)
	g.slots = make(chan struct{}, 2)
	g.BindChannel(ch)
	for _, key := range []string{"one", "two", "three", "four", "five"} {
		input := revivalFixture()
		input.ConversationID = key
		if err := g.QueueRecovery(t.Context(), book, key, input, ""); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		w := &heldRecoveryWorkers{}
		if err := g.ReconcileQueued(t.Context(), book, p, func(string, string) error { return nil }, w); err != nil {
			t.Fatal(err)
		}
		if len(w.jobs) > cap(g.slots) {
			t.Fatal("runtime created a worker without capacity")
		}
		for _, run := range w.jobs {
			run()
		}
	}
	pending, err := book.PendingCommands(t.Context(), recoveryInputKind)
	if err != nil || len(pending) != 5 || ch.notices.Load() != 5 || p.calls.Load() != 0 {
		t.Fatalf("unknown head receipts starved later inputs or were cleared/retried: pending=%d notices=%d calls=%d err=%v", len(pending), ch.notices.Load(), p.calls.Load(), err)
	}
}

// recoveryOwnership reads the claimed inputs, the messages in flight per
// conversation and the taken conversation slots.
func recoveryOwnership(g *Gateway) (claims int, serving map[string]int, slots int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.durableRunning), maps.Clone(g.serving), len(g.slots)
}

// pendingRecoveryIDs lists the recovery inputs not yet acknowledged, oldest
// first.
func pendingRecoveryIDs(t *testing.T, book *ledger.Ledger) []string {
	t.Helper()
	pending, err := book.PendingCommands(t.Context(), recoveryInputKind)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(pending))
	for _, receipt := range pending {
		ids = append(ids, receipt.ID)
	}
	return ids
}

// ReconcileQueued claims an input before it hands the input's run to
// workers, so heldRecoveryWorkers keep every claim of a pass in place until
// the test runs the collected jobs.
func TestGatewayRecoveryClaimTakesItsConversationSlot(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &recoveryProbe{}
	g := New(p)
	g.slots = make(chan struct{}, 2)
	g.BindChannel(&recoveryChannel{})
	for _, key := range []string{"first", "second", "third"} {
		input := revivalFixture()
		input.ConversationID = key
		if err := g.QueueRecovery(t.Context(), book, key, input, ""); err != nil {
			t.Fatal(err)
		}
	}
	revive := func(string, string) error { return nil }
	// A pass hands at most cap(g.slots) runs to workers, so only the second
	// pass offers "third" a claim while "first" and "second" hold both slots.
	w := &heldRecoveryWorkers{}
	for range 2 {
		if err := g.ReconcileQueued(t.Context(), book, p, revive, w); err != nil {
			t.Fatal(err)
		}
	}
	claims, serving, slots := recoveryOwnership(g)
	if want := map[string]int{"first": 1, "second": 1}; len(w.jobs) != 2 || claims != 2 || slots != 2 || !maps.Equal(serving, want) {
		t.Errorf("recovery claims did not each hold a conversation slot: jobs=%d claims=%d serving=%v slots=%d, want jobs=2 claims=2 serving=%v slots=2",
			len(w.jobs), claims, serving, slots, want)
	}
	for _, run := range w.jobs {
		run()
	}
	if _, found, err := book.CommandReceipt(t.Context(), "third/dispatch"); err != nil || found {
		t.Errorf("input without a free slot acquired a dispatch receipt: found=%v err=%v", found, err)
	}
	if pending := pendingRecoveryIDs(t, book); !slices.Equal(pending, []string{"third"}) || p.calls.Load() != 2 {
		t.Fatalf("held runs finished: pending=%v calls=%d, want pending=[third] calls=2", pending, p.calls.Load())
	}
	// Both runs returned their slots, so the next pass claims "third".
	w = &heldRecoveryWorkers{}
	if err := g.ReconcileQueued(t.Context(), book, p, revive, w); err != nil {
		t.Fatal(err)
	}
	for _, run := range w.jobs {
		run()
	}
	if pending := pendingRecoveryIDs(t, book); len(w.jobs) != 1 || len(pending) != 0 || p.calls.Load() != 3 {
		t.Fatalf("returned slots did not admit third: jobs=%d pending=%v calls=%d", len(w.jobs), pending, p.calls.Load())
	}
	if claims, serving, slots := recoveryOwnership(g); claims != 0 || len(serving) != 0 || slots != 0 {
		t.Fatalf("finished runs kept ownership: claims=%d serving=%v slots=%d", claims, serving, slots)
	}
}

type recoveryControlProbe struct {
	blockedRecoveryProbe
	control chan struct{}
}

func (p *recoveryControlProbe) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	if req.Input == "/cancel" {
		close(p.control)
		return turn.Result{Text: "stopped"}, nil
	}
	return p.blockedRecoveryProbe.Handle(ctx, req)
}

func TestGatewayRecoverySlotDoesNotBlockItsOrdinaryStop(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &recoveryControlProbe{blockedRecoveryProbe: blockedRecoveryProbe{entered: make(chan struct{}, 2), release: make(chan struct{})}, control: make(chan struct{})}
	g := New(p)
	g.slots = make(chan struct{}, 1)
	g.BindChannel(&recoveryChannel{})
	if err := g.QueueRecovery(t.Context(), book, "original", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		done <- g.recoverQueuedFixture(t.Context(), book, p, func(string, string) error { return nil })
	}()
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery never entered")
	}
	go func() {
		done <- g.serveTask(feishu.InboundMessage{ConversationID: "conversation", ChatID: "chat", MessageID: "stop", SenderOpenID: "owner", Text: "/cancel"}, "conversation", "")
	}()
	blocked := false
	select {
	case <-p.control:
	case <-time.After(200 * time.Millisecond):
		blocked = true
	}
	close(p.release)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Fatal("conversation slot was not released")
		}
	}
	if blocked {
		t.Fatal("ordinary /cancel waited for the recovery turn's own occupied slot")
	}
}

func TestGatewaySameConversationRecoveryDefersOtherInputWithoutDispatch(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &blockedRecoveryProbe{entered: make(chan struct{}, 2), release: make(chan struct{})}
	g := New(p)
	g.BindChannel(&recoveryChannel{})
	for _, key := range []string{"original", "later"} {
		if err := g.QueueRecovery(t.Context(), book, key, revivalFixture(), ""); err != nil {
			t.Fatal(err)
		}
	}
	var workers recoveryTestWorkers
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); workers.Wait() }()
	for range 2 {
		if err := g.ReconcileQueued(ctx, book, p, func(string, string) error { return nil }, &workers); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("first observer never started")
	}
	if p.calls.Load() != 1 {
		t.Fatal("same conversation dispatched another recovery input concurrently")
	}
	if _, exists, err := book.CommandReceipt(t.Context(), "later/dispatch"); err != nil || exists {
		t.Fatalf("waiting input acquired a dispatch receipt: %v %v", exists, err)
	}
	close(p.release)
	workers.Wait()
	if err := g.ReconcileQueued(ctx, book, p, func(string, string) error { return nil }, &workers); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if p.calls.Load() != 2 {
		t.Fatal("conversation ownership did not release its waiting input")
	}
}

func TestGatewayRecoveryClaimDoesNotJoinItsServingConversation(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &recoveryProbe{}
	g := New(p)
	g.slots = make(chan struct{}, 2)
	g.BindChannel(&recoveryChannel{})
	// Both inputs keep revivalFixture's conversation.
	for _, key := range []string{"first", "second"} {
		if err := g.QueueRecovery(t.Context(), book, key, revivalFixture(), ""); err != nil {
			t.Fatal(err)
		}
	}
	revive := func(string, string) error { return nil }
	w := &heldRecoveryWorkers{}
	if err := g.ReconcileQueued(t.Context(), book, p, revive, w); err != nil {
		t.Fatal(err)
	}
	// A slot stays free, so only the conversation's owner keeps "second" out.
	claims, serving, slots := recoveryOwnership(g)
	if want := map[string]int{"conversation": 1}; len(w.jobs) != 1 || claims != 1 || slots != 1 || !maps.Equal(serving, want) {
		t.Errorf("recovery claimed another input of a serving conversation: jobs=%d claims=%d serving=%v slots=%d, want jobs=1 claims=1 serving=%v slots=1",
			len(w.jobs), claims, serving, slots, want)
	}
	for _, run := range w.jobs {
		run()
	}
	if _, found, err := book.CommandReceipt(t.Context(), "second/dispatch"); err != nil || found {
		t.Errorf("input behind its conversation's owner acquired a dispatch receipt: found=%v err=%v", found, err)
	}
	if pending := pendingRecoveryIDs(t, book); !slices.Equal(pending, []string{"second"}) || p.calls.Load() != 1 {
		t.Fatalf("held run finished: pending=%v calls=%d, want pending=[second] calls=1", pending, p.calls.Load())
	}
	// The run released the conversation, so the next pass claims "second".
	w = &heldRecoveryWorkers{}
	if err := g.ReconcileQueued(t.Context(), book, p, revive, w); err != nil {
		t.Fatal(err)
	}
	for _, run := range w.jobs {
		run()
	}
	if pending := pendingRecoveryIDs(t, book); len(w.jobs) != 1 || len(pending) != 0 || p.calls.Load() != 2 {
		t.Fatalf("released conversation did not admit second: jobs=%d pending=%v calls=%d", len(w.jobs), pending, p.calls.Load())
	}
	if claims, serving, slots := recoveryOwnership(g); claims != 0 || len(serving) != 0 || slots != 0 {
		t.Fatalf("finished runs kept ownership: claims=%d serving=%v slots=%d", claims, serving, slots)
	}
}
