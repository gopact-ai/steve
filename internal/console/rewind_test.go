package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
)

// rewindable is an echo handler that also ends the sessions a thread
// holds, the way the coordinator does.
type rewindable struct {
	mu     sync.Mutex
	seen   []turn.Request
	resets []string
	fail   error
}

func (r *rewindable) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req)
	return turn.Result{Text: "echo: " + req.Input}, nil
}

func (r *rewindable) ResetConversationSessions(_ context.Context, conversation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.resets = append(r.resets, conversation)
	return nil
}

func (r *rewindable) prompts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, req := range r.seen {
		out = append(out, req.Input)
	}
	return out
}

var errNodeGone = errors.New("node is unreachable")

// waitIdle waits for the queue of a conversation to settle, the way a
// page waits for the turn it started.
func waitIdle(t *testing.T, s *Service, conversation string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		for _, e := range s.Queue(conversation) {
			if !e.State.Terminal() {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never went idle", conversation)
}

func sentIDs(list []consoleapi.Reply) []string {
	var out []string
	for _, reply := range list {
		if reply.Kind == "sent" {
			out = append(out, reply.ID)
		}
	}
	return out
}

// Editing a line already sent is not a second question: the thread goes
// back to that line, everything after it leaves the transcript, the
// agent's session is ended so it cannot remember what was removed, and
// what was said before is handed to the new session as text.
func TestRewindTakesTheThreadBackToTheEditedLine(t *testing.T) {
	h := &rewindable{}
	s := New(h, "ou_owner", readmodel.New(readmodel.Sources{}))
	ctx := context.Background()
	for _, line := range []string{"第一个问题", "第二个问题", "第三个问题"} {
		if _, err := s.Send(ctx, "main", line); err != nil {
			t.Fatalf("send %q: %v", line, err)
		}
	}
	second := sentIDs(s.Replies("main"))[1]

	if _, err := s.Submit(ctx, consoleapi.Submission{Conversation: "main", Input: "@claude 改过的第二个问题", CommandID: "c1", RewindTo: second}); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	waitIdle(t, s, "console:main")

	if len(h.resets) != 1 || h.resets[0] != "console:main" {
		t.Fatalf("the agent session was not ended: %v", h.resets)
	}
	var said []string
	for _, reply := range s.Replies("main") {
		if reply.Kind == "sent" {
			said = append(said, reply.Input)
		}
	}
	if len(said) != 2 || said[0] != "第一个问题" || said[1] != "@claude 改过的第二个问题" {
		t.Fatalf("the thread did not go back to the edited line: %v", said)
	}
	if got := len(s.Replies("main")); got != 4 {
		t.Fatalf("the answers to the removed lines are still here: %d replies", got)
	}

	prompts := h.prompts()
	last := prompts[len(prompts)-1]
	// The address has to stay in front, or the rewritten line reaches a
	// different agent than the one it names.
	if !strings.HasPrefix(last, "@claude ") || !strings.HasSuffix(last, "改过的第二个问题") {
		t.Fatalf("the edited line is not what the agent was asked: %q", last)
	}
	if !strings.Contains(last, "第一个问题") || !strings.Contains(last, "echo: 第一个问题") {
		t.Fatalf("the new session was not given what was said before: %q", last)
	}
	if strings.Contains(last, "第三个问题") || strings.Contains(last, "echo: 第二个问题") {
		t.Fatalf("the new session was given lines the thread no longer has: %q", last)
	}

	// The same submission arriving twice must not rewind twice: the
	// exchange it already created is the proof it happened.
	before := len(s.Replies("main"))
	if _, err := s.Submit(ctx, consoleapi.Submission{Conversation: "main", Input: "@claude 改过的第二个问题", CommandID: "c1", RewindTo: second}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if after := len(s.Replies("main")); after != before || len(h.resets) != 1 {
		t.Fatalf("a retried rewind ran again: replies %d -> %d, resets %v", before, after, h.resets)
	}

	// A line that is no longer there is refused by name, so the page can
	// say so instead of failing every later send.
	if _, err := s.Submit(ctx, consoleapi.Submission{Conversation: "main", Input: "再来一次", CommandID: "c2", RewindTo: second}); !errors.Is(err, consoleapi.ErrRewindTargetGone) {
		t.Fatalf("err = %v", err)
	}
}

// A rewind that cannot end the agent's session changes nothing: leaving
// the thread truncated while the agent still remembers the removed turns
// is the one outcome worth refusing for.
func TestRewindKeepsTheThreadWhenTheSessionCannotBeEnded(t *testing.T) {
	h := &rewindable{}
	s := New(h, "ou_owner", readmodel.New(readmodel.Sources{}))
	ctx := context.Background()
	if _, err := s.Send(ctx, "main", "第一个问题"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, "main", "第二个问题"); err != nil {
		t.Fatal(err)
	}
	before := s.Replies("main")
	h.fail = errNodeGone
	_, err := s.Submit(ctx, consoleapi.Submission{Conversation: "main", Input: "改过的", CommandID: "c1", RewindTo: sentIDs(before)[0]})
	if err == nil || !strings.Contains(err.Error(), errNodeGone.Error()) {
		t.Fatalf("err = %v", err)
	}
	if after := s.Replies("main"); len(after) != len(before) {
		t.Fatalf("the transcript was changed by a refused rewind: %d -> %d", len(before), len(after))
	}
}

// blocking is a handler that holds its turn open until released.
type blocking struct {
	rewindable
	gate chan struct{}
}

func (b *blocking) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	if b.gate != nil {
		select {
		case <-b.gate:
		case <-ctx.Done():
			return turn.Result{}, ctx.Err()
		}
	}
	return b.rewindable.Handle(ctx, req)
}

// Truncating a thread under a running turn would leave an agent
// answering into lines that no longer exist, so a rewind waits for the
// thread to be idle and says why.
func TestRewindIsRefusedWhileATurnIsInFlight(t *testing.T) {
	h := &blocking{gate: make(chan struct{})}
	s := New(h, "ou_owner", readmodel.New(readmodel.Sources{}))
	ctx := context.Background()
	close(h.gate)
	if _, err := s.Send(ctx, "main", "第一个问题"); err != nil {
		t.Fatal(err)
	}
	first := sentIDs(s.Replies("main"))[0]

	h.gate = make(chan struct{})
	if _, err := s.Enqueue(ctx, "main", "第二个问题", nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.Submit(ctx, consoleapi.Submission{Conversation: "main", Input: "改过的", CommandID: "c1", RewindTo: first})
	if !errors.Is(err, consoleapi.ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	if len(h.resets) != 0 {
		t.Fatalf("a refused rewind still ended the agent session: %v", h.resets)
	}
	close(h.gate)
	waitIdle(t, s, "console:main")
	if _, err := s.Submit(ctx, consoleapi.Submission{Conversation: "main", Input: "改过的", CommandID: "c1", RewindTo: first}); err != nil {
		t.Fatalf("rewind once idle: %v", err)
	}
}
