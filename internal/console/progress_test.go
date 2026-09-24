package console

import (
	"context"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"github.com/gopact-ai/steve/internal/view"
)

func progressTestStream(t *testing.T) (func(view.Progress), <-chan readmodel.Event) {
	t.Helper()
	model := readmodel.New(readmodel.Sources{})
	events, stop := model.Subscribe(context.Background())
	t.Cleanup(stop)
	s := New(turntest.IdleCoordinator{}, "owner", model)
	stream := s.progress("console:test", "exchange", newProcess())
	t.Cleanup(stream.Close)
	return stream.Update, events
}

func requireProgress(t *testing.T, events <-chan readmodel.Event, within time.Duration) readmodel.Event {
	t.Helper()
	select {
	case ev := <-events:
		if ev.Kind != "console.progress" {
			t.Fatalf("event = %s", ev.Kind)
		}
		return ev
	case <-time.After(within):
		t.Fatal("progress did not arrive before the next agent chunk")
		return readmodel.Event{}
	}
}

func TestProgressFirstContentAfterEmptyIsImmediate(t *testing.T) {
	update, events := progressTestStream(t)
	update(view.Progress{})
	// An empty notification may be ignored or published, but must not use
	// the first-content repaint slot.
	select {
	case <-events:
	default:
	}
	update(view.Progress{Answer: "hello"})
	ev := requireProgress(t, events, 30*time.Millisecond)
	if ev.Progress.Answer != "hello" {
		t.Fatalf("answer = %q", ev.Progress.Answer)
	}
}

func TestProgressPublishesTrailingChunkWhileAgentIsSilent(t *testing.T) {
	update, events := progressTestStream(t)
	update(view.Progress{Answer: "first"})
	requireProgress(t, events, 30*time.Millisecond)
	update(view.Progress{Answer: "first and last"})
	// The agent may now pause for a second or longer. No further callback
	// should be needed for the text already received to become visible.
	ev := requireProgress(t, events, 250*time.Millisecond)
	if ev.Progress.Answer != "first and last" {
		t.Fatalf("answer = %q", ev.Progress.Answer)
	}
}

func TestProgressToolStatusChangeIsImmediate(t *testing.T) {
	update, events := progressTestStream(t)
	update(view.Progress{Tools: []view.Tool{{ID: "tool", Status: view.ToolRunning}}})
	requireProgress(t, events, 30*time.Millisecond)
	update(view.Progress{Tools: []view.Tool{{ID: "tool", Status: view.ToolRunning, Output: "working"}}})
	select {
	case <-events:
	default:
	}
	update(view.Progress{Tools: []view.Tool{{ID: "tool", Status: view.ToolCompleted}}})
	ev := requireProgress(t, events, 30*time.Millisecond)
	if ev.Progress.Tools[0].Status != "completed" {
		t.Fatalf("tool = %+v", ev.Progress.Tools)
	}
}

func TestProgressCloseFlushesBeforeReplyAndRejectsLateCallbacks(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	events, stop := model.Subscribe(context.Background())
	defer stop()
	work := newProcess()
	stream := New(turntest.IdleCoordinator{}, "owner", model).progress("console:test", "exchange", work)
	stream.Update(view.Progress{Answer: "first"})
	requireProgress(t, events, 30*time.Millisecond)
	stream.Update(view.Progress{Answer: "complete"})
	stream.Close()
	ev := requireProgress(t, events, 30*time.Millisecond)
	if ev.Progress.Answer != "complete" {
		t.Fatalf("last progress = %+v", ev.Progress)
	}
	// Late callbacks may race cancellation or a resumed execution ending.
	// Neither may repaint or mutate the process already used for the reply.
	stream.Update(view.Progress{Answer: "late"})
	stream.Phase(view.PhaseRunning)
	stream.Close()
	select {
	case ev := <-events:
		t.Fatalf("progress after close: %+v", ev)
	case <-time.After(2 * progressEvery):
	}
	if work.last.Answer != "complete" {
		t.Fatalf("closed process mutated: %+v", work.last)
	}
}

func TestProgressCarriesTheTaskOnceBound(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	events, stop := model.Subscribe(context.Background())
	defer stop()
	stream := New(turntest.IdleCoordinator{}, "owner", model).progress("console:test", "exchange", newProcess())
	defer stream.Close()
	stream.Update(view.Progress{Answer: "before"})
	if ev := requireProgress(t, events, 30*time.Millisecond); ev.TaskID != "" {
		t.Fatalf("task before the turn is ready = %q", ev.TaskID)
	}
	stream.Bind("12")
	stream.Update(view.Progress{Answer: "after"})
	stream.Close()
	if ev := requireProgress(t, events, 30*time.Millisecond); ev.TaskID != "12" || ev.Progress.Answer != "after" {
		t.Fatalf("bound progress = %+v", ev)
	}
}

func TestProgressFinishingFlushesLatestContentBeforeSlowSave(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	events, stop := model.Subscribe(context.Background())
	defer stop()
	stream := New(turntest.IdleCoordinator{}, "owner", model).progress("console:test", "exchange", newProcess())
	defer stream.Close()
	stream.Phase(view.PhaseWaking)
	requireProgress(t, events, 30*time.Millisecond)
	stream.Update(view.Progress{Answer: "first"})
	requireProgress(t, events, 30*time.Millisecond)
	stream.Update(view.Progress{Answer: "complete"})
	stream.Phase(view.PhaseFinishing)
	ev := requireProgress(t, events, 30*time.Millisecond)
	if ev.Progress.Answer != "complete" || ev.Progress.Phase != "finishing" {
		t.Fatalf("finishing = %+v", ev.Progress)
	}
	stream.Phase(view.PhaseSaving)
	ev = requireProgress(t, events, 30*time.Millisecond)
	if ev.Progress.Answer != "complete" || ev.Progress.Phase != "saving" {
		t.Fatalf("saving = %+v", ev.Progress)
	}
}

// Preparation is the part of a turn with nothing to show: the reader is
// told which step it is on, and told nothing stale once work begins.
func TestProgressStageRidesWakingAndClearsWhenWorkBegins(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	events, stop := model.Subscribe(context.Background())
	defer stop()
	stream := New(turntest.IdleCoordinator{}, "owner", model).progress("console:test", "exchange", newProcess())
	defer stream.Close()
	stream.Phase(view.PhaseWaking)
	requireProgress(t, events, 30*time.Millisecond)
	stream.Stage(view.StageWorkspace)
	if ev := requireProgress(t, events, 30*time.Millisecond); ev.Progress.Stage != "workspace" || ev.Progress.Phase != "waking" {
		t.Fatalf("workspace stage = %+v", ev.Progress)
	}
	stream.Stage(view.StageSession)
	if ev := requireProgress(t, events, 30*time.Millisecond); ev.Progress.Stage != "session" {
		t.Fatalf("session stage = %+v", ev.Progress)
	}
	// Content arriving mid-preparation must not drop the step it is on.
	stream.Update(view.Progress{Answer: "first words"})
	if ev := requireProgress(t, events, 30*time.Millisecond); ev.Progress.Stage != "session" || ev.Progress.Answer != "first words" {
		t.Fatalf("stage across an update = %+v", ev.Progress)
	}
	stream.Phase(view.PhaseRunning)
	if ev := requireProgress(t, events, 30*time.Millisecond); ev.Progress.Stage != "" || ev.Progress.Phase != "running" {
		t.Fatalf("running must not keep a preparation step: %+v", ev.Progress)
	}
}

func TestProgressConcurrentCloseIsPublicationBarrier(t *testing.T) {
	var afterClose atomic.Bool
	var late atomic.Int32
	stream := &progressStream{work: newProcess(), publish: func(consoleapi.Progress) {
		if afterClose.Load() {
			late.Add(1)
		}
	}}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				stream.Update(view.Progress{Answer: "streamed text"})
				stream.Phase(view.PhaseRunning)
			}
		}()
	}
	stream.Close()
	afterClose.Store(true)
	workers.Wait()
	time.Sleep(2 * progressEvery)
	if late.Load() != 0 {
		t.Fatalf("%d events after Close", late.Load())
	}
}

// This goes through the console queue/handler boundary, where slow agent
// finalization used to hide the tail until the terminal reply was saved.
func TestConsoleFinalProgressArrivesDuringFinalization(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	events, stop := model.Subscribe(context.Background())
	defer stop()
	finalizing := make(chan struct{})
	release := make(chan struct{})
	s := New(stepHandler{answer: func(req turn.Request) turn.Result {
		req.OnPhase(view.PhaseRunning)
		req.OnProgress(view.Progress{Answer: "first"})
		req.OnProgress(view.Progress{Answer: "complete"})
		req.OnPhase(view.PhaseFinishing)
		close(finalizing)
		<-release
		return turn.Result{Text: "complete"}
	}}, "owner", model)
	done := make(chan error, 1)
	go func() {
		_, err := s.Send(context.Background(), "test", "hello")
		done <- err
	}()
	<-finalizing
	found := false
	deadline := time.After(250 * time.Millisecond)
	for !found {
		select {
		case ev := <-events:
			if ev.Kind == "console.reply" {
				t.Fatal("reply completed during finalization")
			}
			if ev.Kind == "console.progress" && ev.Progress.Phase == "finishing" {
				if ev.Progress.Answer != "complete" {
					t.Fatalf("final progress = %+v", ev.Progress)
				}
				found = true
			}
		case <-deadline:
			close(release)
			<-done
			t.Fatal("latest answer stayed hidden until finalization finished")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	replied := false
	for !replied {
		select {
		case ev := <-events:
			replied = ev.Kind == "console.reply"
		case <-time.After(time.Second):
			t.Fatal("reply missing")
		}
	}
	select {
	case ev := <-events:
		if ev.Kind == "console.progress" {
			t.Fatalf("progress after terminal reply: %+v", ev)
		}
	case <-time.After(2 * progressEvery):
	}
}

// The model closes a subscriber that falls behind. Following a turn's plan
// must outlive that: progress after the gap is still collected.
func TestFollowKeepsCollectingAfterFallingBehind(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	s := New(turntest.IdleCoordinator{}, "owner", model)
	work := newProcess()
	stop := s.follow(context.Background(), "console:test", work)
	defer stop()
	step := func(id string) {
		model.Publish(readmodel.Event{Kind: "step.progress", Conversation: "console:test", StepID: id, Progress: &consoleapi.Progress{Answer: id}})
	}

	// Hold the collector so it cannot read while far more than its buffer
	// is published.
	work.mu.Lock()
	for range 200 {
		step("behind")
	}
	work.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for {
		step("after")
		work.mu.Lock()
		_, seen := work.steps["after"]
		work.mu.Unlock()
		if seen {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("progress published after falling behind was not collected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A step whose last snapshot is published while the follower is being
// dropped for falling behind still ends at that snapshot: following again
// resumes after the last event read, not at whatever comes next.
func TestFollowKeepsTheLastSnapshotPublishedWhileItWasBehind(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	s := New(turntest.IdleCoordinator{}, "owner", model)
	work := newProcess()
	stop := s.follow(context.Background(), "console:test", work)
	defer stop()
	step := func(id, answer string) {
		model.Publish(readmodel.Event{Kind: "step.progress", Conversation: "console:test", StepID: id, Progress: &consoleapi.Progress{Answer: answer}})
	}
	collect := func(id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			step(id, id)
			work.mu.Lock()
			_, seen := work.steps[id]
			work.mu.Unlock()
			if seen {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("step %s was never collected", id)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// The follower is subscribed once it has collected a step.
	collect("before")

	// Holding the collector, publish far more than its buffer: the model
	// closes the follower before A's last snapshot, which only a resumed
	// subscription can still deliver.
	work.mu.Lock()
	for range 100 {
		step("A", "early")
	}
	step("A", "final")
	work.mu.Unlock()
	collect("after")

	work.mu.Lock()
	got := work.steps["A"].Answer
	work.mu.Unlock()
	if got != "final" {
		t.Fatalf("step A = %q after the follower fell behind; want its last snapshot %q", got, "final")
	}
}
