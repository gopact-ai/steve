package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type echo struct{ seen []turn.Request }

func (e *echo) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	e.seen = append(e.seen, req)
	if strings.HasPrefix(req.Input, "/boom") {
		return turn.Result{}, errors.New("no such verb")
	}
	return turn.Result{Title: "T", Text: "echo: " + req.Input}, nil
}

// A console line runs as the owner, in a console conversation, with an
// anchor in that conversation; replies and milestones are kept
// and published on the change stream.
func TestConsoleActsAsTheOwnerAndKeepsTheExchange(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	h := &echo{}
	s := New(h, "ou_owner", model)
	events, stop := model.Subscribe(context.Background())
	defer stop()

	reply, err := s.Send(context.Background(), "main", "/fleet")
	if err != nil || reply.Text != "echo: /fleet" || reply.Kind != "reply" {
		t.Fatalf("reply = %+v err=%v", reply, err)
	}
	req := h.seen[0]
	if req.ConversationID != "console:main" || req.SenderOpenID != "ou_owner" || !strings.HasPrefix(req.MessageID, AnchorMark) || req.ChatID != ChatID {
		t.Fatalf("request = %+v", req)
	}
	if _, err := s.Send(context.Background(), "console:main", "/boom"); err == nil {
		t.Fatal("an error from the coordinator was swallowed")
	}
	// The exchange is on record: sent, reply, sent, reply(error).
	replies := s.Replies("main")
	if len(replies) != 4 || replies[0].Kind != "sent" || replies[3].Error == "" {
		t.Fatalf("replies = %+v", replies)
	}
	// Milestones from agents and notices for the console land here too.
	id, err := (MessageSender{Console: s}).Send(context.Background(), channel.Address{Channel: "console", Conversation: req.ConversationID, Message: req.MessageID}, channel.Message{Content: "phase 1 done"})
	if err != nil || id == "" {
		t.Fatalf("milestone id = %q err=%v", id, err)
	}
	s.Notice(turn.TaskNotice{TaskID: "9", ChatID: ChatID, MessageID: "web-1", Text: "plan finished"})
	replies = s.Replies("main")
	if replies[len(replies)-1].Kind != "notice" || replies[len(replies)-2].Kind != "milestone" {
		t.Fatalf("tail = %+v", replies[len(replies)-2:])
	}
	// Transcript events remain console-kinded; queue invalidations are separate.
	seen := 0
	for seen < 6 {
		ev := <-events
		if ev.Kind == "console.queue" {
			continue
		}
		if !strings.HasPrefix(ev.Kind, "console.") {
			t.Fatalf("unexpected event %+v", ev)
		}
		seen++
	}
	// Without an owner there is nobody to act as.
	if _, err := New(h, "", model).Send(context.Background(), "main", "hi"); err == nil {
		t.Fatal("a console without an owner acted")
	}
}

// memDoc is a durable document that lives for one test.
type memDoc struct {
	mu    sync.Mutex
	raw   []byte
	saved bool
}

func (d *memDoc) Load() ([]byte, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.raw...), d.saved, nil
}
func (d *memDoc) Save(raw []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.raw, d.saved = append([]byte(nil), raw...), true
	return nil
}
func (d *memDoc) Check() error { return nil }

// A restart must not empty the console: the transcript is kept in a
// document and comes back, per conversation, in order, and the page can
// list which conversations exist.
func TestConsoleTranscriptSurvivesARestart(t *testing.T) {
	doc := &memDoc{}
	h := &echo{}
	first := New(h, "ou_owner", nil)
	if err := first.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Send(context.Background(), "main", "/fleet"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Send(context.Background(), "ops", "hello"); err != nil {
		t.Fatal(err)
	}
	if !doc.saved {
		t.Fatal("nothing was written")
	}

	second := New(h, "ou_owner", nil)
	if err := second.Persist(doc); err != nil {
		t.Fatal(err)
	}
	replies := second.Replies("main")
	if len(replies) != 2 || replies[0].Input != "/fleet" || replies[1].Text != "echo: /fleet" {
		t.Fatalf("restored main = %+v", replies)
	}
	if got := second.Conversations(); len(got) != 2 || got[0] != "console:main" || got[1] != "console:ops" {
		t.Fatalf("conversations = %v", got)
	}
	// New lines append after the restored ones and are saved too.
	if _, err := second.Send(context.Background(), "main", "/tasks"); err != nil {
		t.Fatal(err)
	}
	third := New(h, "ou_owner", nil)
	if err := third.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if replies := third.Replies("main"); len(replies) != 4 || replies[2].Input != "/tasks" {
		t.Fatalf("after second restart main = %+v", replies)
	}
}

// streamer is a handler that reports progress the way an agent turn does,
// then answers.
type streamer struct{}

func (streamer) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	if req.OnProgress != nil {
		req.OnProgress(view.Progress{Reasoning: "thinking about it", Tools: []view.Tool{{ID: "t1", Kind: "shell", Name: "ls", Status: view.ToolRunning}}})
		req.OnProgress(view.Progress{Reasoning: "thinking about it more", Tools: []view.Tool{{ID: "t1", Kind: "shell", Name: "ls", Status: view.ToolCompleted}}})
	}
	return turn.Result{Title: "T", Text: "done"}, nil
}

// While a line runs, its progress streams to the page; when it is done,
// the reply keeps the process — the turn's own, and the steps' progress
// that came through the model for this conversation.
func TestConsoleStreamsProgressAndKeepsTheProcess(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	s := New(streamer{}, "ou_owner", model)
	events, stop := model.Subscribe(context.Background())
	defer stop()
	// A plan step on this conversation reports while the line runs.
	go func() {
		time.Sleep(20 * time.Millisecond)
		model.Publish(readmodel.Event{Kind: "step.progress", Conversation: "console:main", StepID: "repair",
			Progress: &consoleapi.Progress{Agent: "builder", Node: "node-a", Tools: []consoleapi.ToolCall{{Kind: "shell", Name: "install", Status: "completed"}}}})
	}()
	slow := New(slowStreamer{}, "ou_owner", model)
	reply, err := slow.Send(context.Background(), "main", "/repair fixer")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Process == nil || len(reply.Process.Steps) != 1 || reply.Process.Steps[0].ID != "repair" || reply.Process.Steps[0].Agent != "builder" {
		t.Fatalf("process = %+v, want the step's progress kept", reply.Process)
	}
	reply, err = s.Send(context.Background(), "main", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Process == nil || reply.Process.Reasoning != "thinking about it more" || len(reply.Process.Tools) != 1 || reply.Process.Tools[0].Status != "completed" {
		t.Fatalf("process = %+v, want the last progress kept", reply.Process)
	}
	// The stream carried progress between sent and reply, and the tool
	// call's change got through the throttle.
	var kinds []string
	deadline := time.After(2 * time.Second)
	for replies := 0; replies < 2; {
		select {
		case ev := <-events:
			kinds = append(kinds, ev.Kind)
			if ev.Kind == "console.reply" {
				replies++
			}
		case <-deadline:
			t.Fatalf("events so far: %v", kinds)
		}
	}
	joined := strings.Join(kinds, " ")
	if !strings.Contains(joined, "console.sent step.progress console.reply") && !strings.Contains(joined, "step.progress") {
		t.Fatalf("kinds = %v", kinds)
	}
	if !strings.Contains(joined, "console.progress") {
		t.Fatalf("no progress streamed: %v", kinds)
	}
	// The process survives in the transcript.
	replies := s.Replies("main")
	if last := replies[len(replies)-1]; last.Process == nil || len(last.Process.Tools) != 1 {
		t.Fatalf("stored reply = %+v", last)
	}
}

// slowStreamer waits long enough for a step's progress to arrive.
type slowStreamer struct{}

func (slowStreamer) Handle(context.Context, turn.Request) (turn.Result, error) {
	time.Sleep(80 * time.Millisecond)
	return turn.Result{Title: "修复", Text: "done"}, nil
}

// waiter is a handler whose turn takes a while and reports whether the
// context it was given was canceled under it.
type waiter struct{ canceled chan bool }

func (w waiter) Handle(ctx context.Context, _ turn.Request) (turn.Result, error) {
	select {
	case <-ctx.Done():
		w.canceled <- true
		return turn.Result{}, ctx.Err()
	case <-time.After(150 * time.Millisecond):
		w.canceled <- false
		return turn.Result{Text: "done anyway"}, nil
	}
}

func TestConsoleTurnOutlivesTheRequest(t *testing.T) {
	w := waiter{canceled: make(chan bool, 1)}
	s := New(w, "ou_owner", readmodel.New(readmodel.Sources{}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel() // the browser tab closed, the proxy gave up
	}()
	reply, err := s.Send(ctx, "main", "a long piece of work")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if <-w.canceled {
		t.Fatal("the request's cancellation reached the agent's turn")
	}
	if reply.Text != "done anyway" {
		t.Fatalf("reply = %q", reply.Text)
	}
}

func TestConsoleResumesATaskAheadOfWhatWaits(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "ou_owner", readmodel.New(readmodel.Sources{}))
	first := enqueueForTest(t, s, "main", "first")
	running := nextCall(t, h)
	later := enqueueForTest(t, s, "main", "later")
	var revived []string
	revive := func(conversation, member string) error {
		revived = append(revived, conversation+"/"+member)
		return nil
	}
	if err := s.Resume(context.Background(), "main", "53", "claude", "⟳ restart #53", "continue: ship it", revive); err != nil {
		t.Fatal(err)
	}
	if len(revived) != 1 || revived[0] != "console:main/claude" {
		t.Fatalf("revived = %v", revived)
	}
	// Behind the line that runs, ahead of the one that waits; the page
	// shows the notice, the agent gets the continuation.
	list := s.Queue("main")
	if len(list) != 3 || list[0].ID != first.ID || list[1].Input != "⟳ restart #53" || list[1].Prompt != "@claude continue: ship it" || list[2].ID != later.ID {
		t.Fatalf("queue = %+v", list)
	}
	running.finish <- nil
	call := nextCall(t, h)
	if call.req.Input != "@claude continue: ship it" || call.req.ConversationID != "console:main" {
		t.Fatalf("continuation = %+v", call.req)
	}
	call.finish <- nil
	next := nextCall(t, h)
	if next.req.Input != "later" {
		t.Fatalf("after the continuation came %q", next.req.Input)
	}
	next.finish <- nil
	awaitExchange(t, s, later.ID)
	var sent []string
	for _, r := range s.Replies("main") {
		if r.Kind == "sent" {
			sent = append(sent, r.Input)
		}
	}
	if strings.Join(sent, "|") != "first|⟳ restart #53|later" {
		t.Fatalf("sent lines = %v", sent)
	}
	// Without a member there is nobody to continue; the session is left alone.
	if err := s.Resume(context.Background(), "main", "54", "", "n", "p", revive); err == nil || len(revived) != 1 {
		t.Fatalf("err = %v revived = %v", err, revived)
	}
}

func TestNoticeLandsInTheTasksOwnThread(t *testing.T) {
	s := New(&echo{}, "ou_owner", readmodel.New(readmodel.Sources{}))
	s.Notice(turn.TaskNotice{TaskID: "56", ChatID: ChatID, Conversation: "console:e2e-fleet-1", Text: "done"})
	s.Notice(turn.TaskNotice{TaskID: "57", ChatID: ChatID, Text: "no thread known"})
	if got := s.Replies("e2e-fleet-1"); len(got) != 1 || got[0].Kind != "notice" || got[0].Title != "task #56" {
		t.Fatalf("thread of the task = %+v", got)
	}
	if got := s.Replies("main"); len(got) != 1 || got[0].Title != "task #57" {
		t.Fatalf("main = %+v", got)
	}
}

func TestContinueHappensOnceForAKey(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "ou_owner", readmodel.New(readmodel.Sources{}))
	first := enqueueForTest(t, s, "main", "first")
	running := nextCall(t, h)
	later := enqueueForTest(t, s, "main", "later")
	for range 3 {
		if err := s.Continue(context.Background(), "main", "deliver:59", "claude", "⤵ 子任务 #59 完成", "[steve] child done"); err != nil {
			t.Fatal(err)
		}
	}
	list := s.Queue("main")
	if len(list) != 3 || list[0].ID != first.ID || list[1].Key != "deliver:59" || list[1].Prompt != "@claude [steve] child done" || list[2].ID != later.ID {
		t.Fatalf("queue = %+v", list)
	}
	running.finish <- nil
	call := nextCall(t, h)
	if call.req.Input != "@claude [steve] child done" {
		t.Fatalf("continuation = %q", call.req.Input)
	}
	call.finish <- nil
	// The same key after it ran is still one exchange, not a second.
	if err := s.Continue(context.Background(), "main", "deliver:59", "claude", "again", "again"); err != nil {
		t.Fatal(err)
	}
	next := nextCall(t, h)
	if next.req.Input != "later" {
		t.Fatalf("after the continuation came %q", next.req.Input)
	}
	next.finish <- nil
	awaitExchange(t, s, later.ID)
	keyed := 0
	for _, e := range s.Queue("main") {
		if e.Key == "deliver:59" {
			keyed++
		}
	}
	if keyed != 1 {
		t.Fatalf("%d exchanges carry the key", keyed)
	}
	if err := s.Continue(context.Background(), "main", "deliver:60", "", "n", "p"); err == nil {
		t.Fatal("a continuation without a member was accepted")
	}
}

func TestAConsoleTurnIsAnchoredSoMilestonesLandInItsThread(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "ou_owner", readmodel.New(readmodel.Sources{}))
	var anchored []string
	s.SetAnchorer(func(conversation, chatID, messageID string) {
		anchored = append(anchored, conversation+"|"+chatID+"|"+messageID)
	})
	e := enqueueForTest(t, s, "side", "do it")
	call := nextCall(t, h)
	if len(anchored) != 1 || anchored[0] != "console:side|"+ChatID+"|"+AnchorMark+e.ID {
		t.Fatalf("anchored = %v", anchored)
	}
	// An agent's progress message during the turn lands in this thread.
	if _, err := (MessageSender{Console: s}).Send(context.Background(), channel.Address{Channel: "console", Conversation: e.Conversation, Message: AnchorMark + e.ID}, channel.Message{Content: "halfway"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Replies("side"); len(got) != 2 || got[1].Kind != "milestone" || got[1].Text != "halfway" {
		t.Fatalf("side thread = %+v", got)
	}
	if got := s.Replies("main"); len(got) != 0 {
		t.Fatalf("main got the milestone: %+v", got)
	}
	call.finish <- nil
	awaitExchange(t, s, e.ID)
}
