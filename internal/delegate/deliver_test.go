package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// A parent that ends its turn is told when a child ends: no polling.

type mailbox struct {
	mu   sync.Mutex
	got  []Delivery
	fail error
}

func (m *mailbox) deliver(_ context.Context, d Delivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.got = append(m.got, d)
	return nil
}

func (m *mailbox) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.got)
}

func (m *mailbox) wait(t *testing.T, n int) []Delivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		if len(m.got) >= n {
			out := append([]Delivery(nil), m.got...)
			m.mu.Unlock()
			return out
		}
		m.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited for %d deliveries, have %d", n, m.count())
	return nil
}

// startBlocked delegates a child whose turn waits until release is
// closed, so the test controls when it ends.
func startBlocked(t *testing.T, w *world, member string) (agentmcp.DelegateResult, func()) {
	t.Helper()
	release := make(chan struct{})
	w.sessions.run = func(ctx context.Context, _ func(view.Progress)) (string, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "done it", nil
	}
	w.service.InlineWait = 50 * time.Millisecond
	first, err := w.service.Start(t.Context(), "chat", member, agentmcp.DelegateRequest{Goal: "write it", Requires: []string{"gpu"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "running" {
		t.Fatalf("child state after placement = %s, want running", first.State)
	}
	var once sync.Once
	return first, func() { once.Do(func() { close(release) }) }
}

func TestAChildsResultReachesAnIdleParentOnce(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	first, release := startBlocked(t, w, "codex")
	if !strings.Contains(first.Note, "need not wait") {
		t.Fatalf("placement note = %q", first.Note)
	}
	release()

	got := box.wait(t, 1)
	d := got[0]
	if d.Conversation != "chat" || d.ParentTask != parent.ID || d.Member != "codex" || len(d.Children) != 1 {
		t.Fatalf("delivery = %+v", d)
	}
	c := d.Children[0]
	if c.Task != first.TaskID || c.State != "done" || c.Answer != "done it" || c.Agent != "builder" || c.Node != "node-a" {
		t.Fatalf("child = %+v", c)
	}
	if !strings.Contains(d.Notice(), "⤵ 子任务 #"+first.TaskID) || !strings.Contains(d.Prompt(), "继续你的任务") || !strings.Contains(d.Prompt(), "done it") {
		t.Fatalf("notice = %q prompt = %q", d.Notice(), d.Prompt())
	}
	if d.Key != task.DeliveryKey(first.TaskID) {
		t.Fatalf("key = %q", d.Key)
	}
	child, _ := w.tasks.Get(first.TaskID)
	if child.Result == nil || child.Result.Answer != "done it" || child.Delivery == nil || child.Delivery.State != task.DeliveryDelivered {
		t.Fatalf("child record = %+v result=%+v delivery=%+v", child, child.Result, child.Delivery)
	}
	// Asking again sends nothing: it was delivered.
	w.service.Flush(t.Context(), parent.ID)
	w.service.RedeliverPending(t.Context())
	if box.count() != 1 {
		t.Fatalf("delivered %d times", box.count())
	}
}

func TestAChildEndingMidTurnWaitsForTheTurnToEnd(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	// The parent's turn holds a live attempt.
	live, err := w.attempts.Open(t.Context(), attempt.Spec{TaskID: parent.ID, Kind: attempt.KindChat, Project: "p", Agent: "codex", Harness: "mock",
		Workspace: project.Workspace{ID: "canonical:p", Project: "p", Path: w.home, Kind: project.KindCanonical}, Scope: attempt.ScopeUnrestricted})
	if err != nil {
		t.Fatal(err)
	}
	first, release := startBlocked(t, w, "codex")
	release()
	// The child ends; the record says so; nothing is delivered yet.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, _ := w.tasks.Get(first.TaskID); c.Finished() && c.Result != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if box.count() != 0 {
		t.Fatalf("delivered %d while the parent's turn ran", box.count())
	}
	// The turn ends: its close delivers.
	if _, err := w.attempts.Fail(t.Context(), live.ID, "turn", "turn over"); err != nil {
		t.Fatal(err)
	}
	w.service.Flush(t.Context(), parent.ID)
	if got := box.wait(t, 1); got[0].Children[0].Task != first.TaskID {
		t.Fatalf("delivery = %+v", got[0])
	}
}

func TestAResultCollectedByAwaitIsNotDeliveredAgain(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	w.sessions.reply = func(string) (string, error) { return "here", nil }
	res, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "write it", Requires: []string{"gpu"}})
	if err != nil || res.State != "done" {
		t.Fatalf("delegate = %+v, %v", res, err)
	}
	w.service.Flush(t.Context(), parent.ID)
	time.Sleep(100 * time.Millisecond)
	if box.count() != 0 {
		t.Fatalf("a collected result was delivered %d time(s)", box.count())
	}
	if c, _ := w.tasks.Get(res.TaskID); c.Delivery == nil || c.Delivery.State != task.DeliveryDelivered {
		t.Fatalf("child delivery = %+v", c.Delivery)
	}
}

func TestAClosedParentGetsNoContinuation(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	first, release := startBlocked(t, w, "codex")
	if _, err := w.tasks.Finish(parent.ID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Advance(parent.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, _ := w.tasks.Get(first.TaskID); c.Delivery != nil && c.Delivery.State == task.DeliveryDelivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child's delivery was never settled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if box.count() != 0 {
		t.Fatalf("a closed parent was sent %d message(s)", box.count())
	}
}

func TestAFailedDeliveryIsRetriedFromTheRecord(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{fail: errors.New("channel down")}
	w.service.SetDeliverer(box.deliver)
	first, release := startBlocked(t, w, "codex")
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, _ := w.tasks.Get(first.TaskID); c.Delivery != nil && c.Delivery.State == task.DeliveryPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failed delivery was not left pending")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The channel comes back; the start-up pass finds the record.
	box.mu.Lock()
	box.fail = nil
	box.mu.Unlock()
	w.service.RedeliverPending(t.Context())
	got := box.wait(t, 1)
	if got[0].ParentTask != parent.ID || got[0].Children[0].Task != first.TaskID {
		t.Fatalf("delivery = %+v", got[0])
	}
}

func TestDeliverySaysWhereTheChildsFilesAre(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	first, release := startBlocked(t, w, "codex")
	// The child writes a file in its own worktree before it ends.
	child, ok := w.tasks.Get(first.TaskID)
	if !ok || child.Workspace == "" {
		t.Fatalf("child workspace unknown: %+v", child)
	}
	if err := os.WriteFile(filepath.Join(child.Workspace, "notes.md"), []byte("by the child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release()
	got := box.wait(t, 1)
	c := got[0].Children[0]
	if !strings.Contains(c.Landing, "已落地") {
		t.Fatalf("landing = %q refs=%v", c.Landing, c.Refs)
	}
	if _, err := os.Stat(filepath.Join(w.home, "notes.md")); err != nil {
		t.Fatalf("the child's file did not reach the main directory: %v", err)
	}
	if !strings.Contains(got[0].Prompt(), "已落地") {
		t.Fatalf("prompt does not say where the files are: %q", got[0].Prompt())
	}
	_ = parent
}
