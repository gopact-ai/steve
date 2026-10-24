package chief

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/thread"
	"github.com/gopact-ai/steve/internal/work"
)

type fakeRunner struct {
	mu      sync.Mutex
	prompts []string
	reply   func(prompt string) (string, error)
	gate    chan struct{}
}

func (r *fakeRunner) Prompt(_ context.Context, spec agentexec.Spec, prompt string, _ func(string) error) (agentexec.Result, error) {
	if r.gate != nil {
		<-r.gate
	}
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
	answer, err := r.reply(prompt)
	return agentexec.Result{Answer: answer}, err
}

type fakeWork struct {
	mu        sync.Mutex
	submitted []work.Request
	stopped   []string
	finished  func(work.Card)
	cards     []work.Card
}

func (w *fakeWork) Submit(_ context.Context, r work.Request) (work.Card, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, prev := range w.submitted {
		if prev.Key == r.Key {
			return work.Card{ID: string(rune('1' + i))}, nil
		}
	}
	w.submitted = append(w.submitted, r)
	return work.Card{ID: string(rune('0' + len(w.submitted)))}, nil
}
func (w *fakeWork) Stop(_ context.Context, id string) (work.Card, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id == "404" {
		return work.Card{}, work.ErrNotFound
	}
	w.stopped = append(w.stopped, id)
	return work.Card{ID: id}, nil
}
func (w *fakeWork) List() []work.Card            { return w.cards }
func (w *fakeWork) OnFinished(f func(work.Card)) { w.finished = f }

type world struct {
	chief  *Chief
	thread *thread.Service
	runner *fakeRunner
	work   *fakeWork
}

func newWorld(t *testing.T, reply func(string) (string, error)) *world {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	threads := thread.New(book, nil)
	r := &fakeRunner{reply: reply}
	w := &fakeWork{}
	c := New(t.Context(), threads, w, tasks, r, Options{Agent: "codex", Project: "p"})
	return &world{chief: c, thread: threads, runner: r, work: w}
}

func (w *world) say(t *testing.T, text string) {
	t.Helper()
	if _, _, err := w.thread.Post(t.Context(), thread.Message{Role: thread.RoleOwner, Text: text, ClientNonce: text}); err != nil {
		t.Fatal(err)
	}
}

func (w *world) messages(t *testing.T) []thread.Message {
	t.Helper()
	all, err := w.thread.Recent(t.Context(), thread.Main, 100)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func TestAnOwnerMessageGetsAReplyAndTheProposedTasksStart(t *testing.T) {
	w := newWorld(t, func(string) (string, error) {
		return "好的，我让 codex 去修。\n```steve-actions\n{\"delegate\":[{\"goal\":\"Fix issue 7 in gopact-ai/steve and open a PR\"}],\"stop\":[\"3\"]}\n```", nil
	})
	w.work.cards = []work.Card{{ID: "3", State: work.StateRunning, Mode: "auto", Goal: "old job", Activity: work.Activity{Line: "go test"}}}
	w.say(t, "修一下 issue 7，顺便把 #3 停了")
	w.chief.Wait()
	all := w.messages(t)
	if len(all) != 2 || all[1].Role != thread.RoleSteve || all[1].Text != "好的，我让 codex 去修。" || len(all[1].Tasks) != 1 || all[1].Tasks[0] != "1" {
		t.Fatalf("thread = %+v", all)
	}
	if len(w.work.submitted) != 1 || w.work.submitted[0].Goal != "Fix issue 7 in gopact-ai/steve and open a PR" || w.work.submitted[0].Thread != thread.Main || w.work.submitted[0].By != "chief" {
		t.Fatalf("submitted = %+v", w.work.submitted)
	}
	if len(w.work.stopped) != 1 || w.work.stopped[0] != "3" {
		t.Fatalf("stopped = %+v", w.work.stopped)
	}
	p := w.runner.prompts[0]
	for _, want := range []string{"#3 [running, auto] old job — now: go test", "[owner] 修一下 issue 7", "steve-actions", "Chinese"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestMessagesDuringATurnJoinTheNextTurn(t *testing.T) {
	gate := make(chan struct{})
	w := newWorld(t, func(string) (string, error) { return "收到", nil })
	w.runner.gate = gate
	w.say(t, "first")
	w.say(t, "second")
	w.say(t, "third")
	gate <- struct{}{}
	gate <- struct{}{}
	w.chief.Wait()
	if n := len(w.runner.prompts); n != 2 {
		t.Fatalf("turns = %d, want 2 (the first, then the two that arrived during it)", n)
	}
	if !strings.Contains(w.runner.prompts[1], "[owner] third") {
		t.Fatal("second turn missed the queued message")
	}
}

func TestAMalformedBlockStartsNothingAndSaysSo(t *testing.T) {
	w := newWorld(t, func(string) (string, error) {
		return "我来处理\n```steve-actions\n{\"delegate\":[{\"goal\":\"x\",\"shell\":\"rm -rf /\"}]}\n```", nil
	})
	w.say(t, "do it")
	w.chief.Wait()
	all := w.messages(t)
	if len(w.work.submitted) != 0 || len(all) != 3 || all[1].Text != "我来处理" || all[2].Role != thread.RoleNotice || !strings.Contains(all[2].Text, "没有执行任何操作") {
		t.Fatalf("thread = %+v submitted=%+v", all, w.work.submitted)
	}
}

func TestAFailedTurnNamesItsClassAndAFailedActionIsReported(t *testing.T) {
	w := newWorld(t, func(string) (string, error) { return "", errors.New("upstream 429 Too Many Requests") })
	w.say(t, "在吗")
	w.chief.Wait()
	all := w.messages(t)
	if len(all) != 2 || all[1].Role != thread.RoleNotice || !strings.Contains(all[1].Text, "模型额度或限流") {
		t.Fatalf("thread = %+v", all)
	}
	w.runner.reply = func(string) (string, error) {
		return "停掉\n```steve-actions\n{\"stop\":[\"404\"]}\n```", nil
	}
	w.say(t, "停掉 404")
	w.chief.Wait()
	all = w.messages(t)
	if last := all[len(all)-1]; last.Role != thread.RoleNotice || !strings.Contains(last.Text, "没能停止") {
		t.Fatalf("thread = %+v", all)
	}
}

func TestATaskOutcomeIsPostedByTheProgram(t *testing.T) {
	w := newWorld(t, func(string) (string, error) { return "", nil })
	w.work.finished(work.Card{ID: "5", Thread: thread.Main, State: work.StateDone, Goal: "fix issue 7", Answer: "Opened https://github.com/gopact-ai/steve/pull/400"})
	w.work.finished(work.Card{ID: "6", Thread: thread.Main, State: work.StateFailed, Failure: work.FailQuota, Goal: "release", Error: "429"})
	w.work.finished(work.Card{ID: "7", Thread: "elsewhere", State: work.StateDone})
	all := w.messages(t)
	if len(all) != 2 || all[0].TaskID != "5" || !strings.Contains(all[0].Text, "任务 #5 已完成：fix issue 7") || !strings.Contains(all[0].Text, "pull/400") || !strings.Contains(all[1].Text, "模型额度或限流") {
		t.Fatalf("thread = %+v", all)
	}
	if len(w.runner.prompts) != 0 {
		t.Fatal("a notice started a turn")
	}
}
