package tuiapp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/gopact-ai/steve/internal/clientapi"
	"github.com/gopact-ai/steve/internal/clientsdk"
)

// fakeAPI is a server holding one thread. Tests change what it holds and
// send the events a real server would.
type fakeAPI struct {
	mu         sync.Mutex
	detail     clientapi.ThreadDetail
	turns      []clientapi.StartTurn
	interrupts []string
	answers    []clientapi.QuestionAnswer
	reads      int
	failStart  error
	// failReads and failProjects fail that many calls before answering.
	failReads    int
	failProjects int
	others       map[string]clientapi.ThreadDetail
}

func newFake() *fakeAPI {
	return &fakeAPI{detail: clientapi.ThreadDetail{Thread: clientapi.Thread{ID: "console:main", Transport: "console", Project: "scratch", Agent: "codex"}}}
}

func (f *fakeAPI) Initialize(context.Context) (clientapi.Initialize, error) {
	return clientapi.Initialize{Protocol: clientapi.Protocol}, nil
}
func (f *fakeAPI) Projects(context.Context) ([]clientapi.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failProjects > 0 {
		f.failProjects--
		return nil, errors.New("connection refused")
	}
	return []clientapi.Project{{ID: "scratch", Default: true}}, nil
}
func (f *fakeAPI) Agents(context.Context) ([]clientapi.Agent, error) {
	return []clientapi.Agent{{ID: "codex", Node: "laptop", Default: true}}, nil
}
func (f *fakeAPI) Threads(context.Context) ([]clientapi.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []clientapi.Thread{f.detail.Thread}
	for _, d := range f.others {
		out = append(out, d.Thread)
	}
	return out, nil
}
func (f *fakeAPI) Thread(_ context.Context, id string, _ clientsdk.Page) (clientapi.ThreadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.failReads > 0 {
		f.failReads--
		return clientapi.ThreadDetail{}, errors.New("connection reset")
	}
	if d, ok := f.others[id]; ok {
		return d, nil
	}
	if id != f.detail.Thread.ID {
		return clientapi.ThreadDetail{}, &clientsdk.Error{Status: 404, Code: clientapi.CodeNotFound}
	}
	d := f.detail
	d.Items = append([]clientapi.Item(nil), f.detail.Items...)
	d.Turns = append([]clientapi.Turn(nil), f.detail.Turns...)
	return d, nil
}
func (f *fakeAPI) CreateThread(_ context.Context, req clientapi.CreateThread) (clientapi.ThreadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := clientapi.ThreadDetail{Thread: clientapi.Thread{ID: "console:" + req.ID, Transport: "console", Project: req.Project}}
	if f.others == nil {
		f.others = map[string]clientapi.ThreadDetail{}
	}
	f.others[d.Thread.ID] = d
	return d, nil
}
func (f *fakeAPI) StartTurn(_ context.Context, req clientapi.StartTurn) (clientapi.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, req)
	if f.failStart != nil {
		return clientapi.Turn{}, f.failStart
	}
	id := "t" + req.CommandID
	f.detail.Items = append(f.detail.Items, clientapi.Item{ID: "u" + req.CommandID, Kind: clientapi.ItemMessage, Role: "user", Text: req.Input, Turn: id})
	turn := clientapi.Turn{ID: id, Thread: req.Thread, State: clientapi.TurnRunning, Input: req.Input, CommandID: req.CommandID}
	f.detail.Turns = append(f.detail.Turns, turn)
	return turn, nil
}
func (f *fakeAPI) Interrupt(_ context.Context, turn, commandID string) (clientapi.InterruptResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts = append(f.interrupts, turn+"/"+commandID)
	return clientapi.InterruptResult{Turn: clientapi.Turn{ID: turn}, Control: &clientapi.Turn{ID: "stop"}}, nil
}
func (f *fakeAPI) Answer(_ context.Context, id string, answer clientapi.QuestionAnswer) (clientapi.AnswerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, answer)
	for i, item := range f.detail.Items {
		if item.Question != nil && item.Question.ID == id {
			q := *item.Question
			q.State, q.Answer = "answered", &answer
			f.detail.Items[i].Question = &q
			return clientapi.AnswerResult{Question: q}, nil
		}
	}
	return clientapi.AnswerResult{}, &clientsdk.Error{Status: 404, Code: clientapi.CodeNotFound}
}
func (f *fakeAPI) Follow(context.Context, string, string) <-chan clientsdk.Update { return nil }

// finish ends a running turn with a reply, as the server records it.
func (f *fakeAPI) finish(turn, reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.detail.Turns {
		if f.detail.Turns[i].ID == turn {
			f.detail.Turns[i].State = clientapi.TurnDone
		}
	}
	f.detail.Items = append(f.detail.Items, clientapi.Item{ID: "r" + turn, Kind: clientapi.ItemMessage, Role: "assistant", Text: reply, Turn: turn})
}

// harness drives a Model the way Bubble Tea does: it applies a message,
// runs the commands it returns and applies their results.
type harness struct {
	t     *testing.T
	model Model
	quit  bool
}

func start(t *testing.T, api API) *harness {
	t.Helper()
	h := &harness{t: t, model: New(t.Context(), api, Options{})}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 50})
	h.run(h.model.Init())
	return h
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	next, cmd := h.model.Update(msg)
	h.model = next.(Model)
	h.run(cmd)
}

// run executes cmd and applies the client's own results. Cursor blinks
// and other library timers are dropped.
func (h *harness) run(cmd tea.Cmd) {
	h.t.Helper()
	for _, msg := range collect(cmd) {
		switch msg.(type) {
		case tea.QuitMsg:
			h.quit = true
		case bootMsg, bootRetryMsg, detailMsg, sentMsg, interruptedMsg, answeredMsg, threadsMsg:
			h.send(msg)
		}
	}
}

func collect(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range batch {
				out = append(out, collect(c)...)
			}
			return out
		}
		return []tea.Msg{msg}
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

func (h *harness) typeText(text string) {
	h.t.Helper()
	for _, r := range text {
		h.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func (h *harness) press(code rune, mod tea.KeyMod) {
	h.t.Helper()
	h.send(tea.KeyPressMsg{Code: code, Mod: mod})
}

func (h *harness) screen() string {
	return ansi.Strip(h.model.View().Content)
}

func (h *harness) event(ev clientapi.Event) {
	h.t.Helper()
	h.send(StreamMsg{Link: clientsdk.Connected, Event: &ev})
}

func TestSendSubmitsTheInputAndShowsTheReply(t *testing.T) {
	api := newFake()
	h := start(t, api)
	h.send(StreamMsg{Link: clientsdk.Connected})
	if !strings.Contains(h.screen(), "project scratch") || !strings.Contains(h.screen(), "agent codex") || !strings.Contains(h.screen(), "node laptop") || !strings.Contains(h.screen(), "connected") {
		t.Fatalf("header does not show the thread's context:\n%s", h.screen())
	}
	h.typeText("hello")
	h.press(tea.KeyEnter, tea.ModShift)
	h.typeText("there")
	h.press(tea.KeyEnter, 0)
	if len(api.turns) != 1 {
		t.Fatalf("turns = %+v", api.turns)
	}
	sent := api.turns[0]
	if sent.Input != "hello\nthere" || sent.Thread != "console:main" || sent.CommandID == "" || sent.ExpectedProject != "scratch" {
		t.Fatalf("Shift+Enter should add a line and Enter send it: %+v", sent)
	}
	if h.model.input.Value() != "" {
		t.Fatalf("input kept %q after sending", h.model.input.Value())
	}
	api.finish("t"+sent.CommandID, "hi back")
	h.event(clientapi.Event{Type: clientapi.EventItemCompleted, Thread: "console:main", Item: "rt" + sent.CommandID})
	screen := h.screen()
	if !strings.Contains(screen, "hello") || !strings.Contains(screen, "hi back") || strings.Contains(screen, "(sending)") {
		t.Fatalf("transcript after the reply:\n%s", screen)
	}
}

// A slash command is a turn's input like any other; the server runs it.
func TestSlashCommandsGoToTheServerVerbatim(t *testing.T) {
	api := newFake()
	h := start(t, api)
	h.typeText("/help")
	h.press(tea.KeyEnter, 0)
	if len(api.turns) != 1 || api.turns[0].Input != "/help" {
		t.Fatalf("turns = %+v", api.turns)
	}
}

// A send the server did not acknowledge stays in the box, and sending it
// again reuses its command_id so it cannot run twice.
func TestFailedSendRetriesWithTheSameCommandID(t *testing.T) {
	api := newFake()
	api.failStart = errors.New("connection refused")
	h := start(t, api)
	h.typeText("deploy")
	h.press(tea.KeyEnter, 0)
	if h.model.input.Value() != "deploy" || !strings.Contains(h.screen(), "not sent") {
		t.Fatalf("a failed send should keep the input and say so: %q\n%s", h.model.input.Value(), h.screen())
	}
	api.failStart = nil
	h.press(tea.KeyEnter, 0)
	if len(api.turns) != 2 || api.turns[0].CommandID != api.turns[1].CommandID {
		t.Fatalf("retry = %+v", api.turns)
	}
}

// Progress events stream the running turn's answer into the transcript
// until the reply item replaces it.
func TestStreamingProgressIsShownUntilTheReply(t *testing.T) {
	api := newFake()
	h := start(t, api)
	h.typeText("explain")
	h.press(tea.KeyEnter, 0)
	turn := "t" + api.turns[0].CommandID
	h.event(clientapi.Event{Type: clientapi.EventTurnProgress, Thread: "console:main", Turn: turn, Progress: &clientapi.Progress{Phase: "thinking", Agent: "codex", Node: "studio",
		Tools: []clientapi.ToolCall{{Name: "read", Detail: "main.go", Status: "completed"}}, Answer: "The answer so"}})
	screen := h.screen()
	if !strings.Contains(screen, "The answer so") || !strings.Contains(screen, "read main.go") || !strings.Contains(screen, "thinking") || !strings.Contains(screen, "node studio") {
		t.Fatalf("progress not shown:\n%s", screen)
	}
	h.event(clientapi.Event{Type: clientapi.EventTurnProgress, Thread: "console:main", Turn: turn, Progress: &clientapi.Progress{Answer: "The answer so far is longer"}})
	if !strings.Contains(h.screen(), "The answer so far is longer") {
		t.Fatalf("progress did not update:\n%s", h.screen())
	}
	// Another thread's progress is not this thread's.
	h.event(clientapi.Event{Type: clientapi.EventTurnProgress, Thread: "console:other", Turn: "x", Progress: &clientapi.Progress{Answer: "elsewhere"}})
	if strings.Contains(h.screen(), "elsewhere") {
		t.Fatal("another thread's progress was shown")
	}
	api.finish(turn, "Final answer")
	h.event(clientapi.Event{Type: clientapi.EventItemCompleted, Thread: "console:main", Item: "r" + turn})
	screen = h.screen()
	if !strings.Contains(screen, "Final answer") || strings.Contains(screen, "(writing)") {
		t.Fatalf("the reply should replace the progress:\n%s", screen)
	}
}

// The connection state stays visible however long the thread's context
// is, and a failed reply whose error repeats its text is shown once.
func TestNarrowHeaderKeepsTheConnectionState(t *testing.T) {
	api := newFake()
	api.detail.Thread.Title = strings.Repeat("a long thread title ", 5)
	api.detail.Items = []clientapi.Item{{ID: "r1", Kind: clientapi.ItemMessage, Role: "assistant", Text: "No agent is registered.", Error: "No agent is registered."}}
	h := start(t, api)
	h.send(tea.WindowSizeMsg{Width: 40, Height: 20})
	h.send(StreamMsg{Link: clientsdk.Connected})
	header := strings.SplitN(h.screen(), "\n", 2)[0]
	if !strings.Contains(header, "connected") {
		t.Fatalf("header %q lost the connection state", header)
	}
	if n := strings.Count(h.screen(), "No agent is registered."); n != 1 {
		t.Fatalf("the reply is shown %d times:\n%s", n, h.screen())
	}
}

func pendingQuestion() clientapi.Item {
	q := &clientapi.Question{ID: "q1", Thread: "console:main", Turn: "t0", Kind: "permission", Title: "Run a command", Message: "Allow `go test`?", State: questionPending,
		Options: []clientapi.QuestionOption{{ID: "allow", Label: "Allow"}, {ID: "always", Label: "Always allow"}, {ID: "reject", Label: "Reject", Kind: "reject_once"}}}
	return clientapi.Item{ID: "question:q1", Kind: clientapi.ItemQuestion, Thread: "console:main", Turn: "t0", Question: q}
}

func TestQuestionOverlayAnswersThroughTheProtocol(t *testing.T) {
	api := newFake()
	api.detail.Turns = []clientapi.Turn{{ID: "t0", State: clientapi.TurnRunning, Phase: "awaiting-user"}}
	api.detail.Items = []clientapi.Item{pendingQuestion()}
	h := start(t, api)
	screen := h.screen()
	if !strings.Contains(screen, "Run a command") || !strings.Contains(screen, "Allow `go test`?") || !strings.Contains(screen, "› Allow") {
		t.Fatalf("question overlay not shown:\n%s", screen)
	}
	// The overlay has the keyboard: typing does not reach the input box.
	h.typeText("x")
	if h.model.input.Value() != "" {
		t.Fatalf("keys reached the input box: %q", h.model.input.Value())
	}
	h.press(tea.KeyDown, 0)
	h.press(tea.KeyEnter, 0)
	if len(api.answers) != 1 || api.answers[0].Decision != "accept" || api.answers[0].Choice != "always" || api.answers[0].CommandID == "" {
		t.Fatalf("answers = %+v", api.answers)
	}
	if strings.Contains(h.screen(), "↑/↓ choose") || h.model.focus != focusInput {
		t.Fatalf("the overlay should close once the question is answered:\n%s", h.screen())
	}
}

func TestQuestionCanBeDeclinedOrPutOff(t *testing.T) {
	api := newFake()
	api.detail.Items = []clientapi.Item{pendingQuestion()}
	h := start(t, api)
	h.press(tea.KeyEscape, 0)
	if h.model.focus != focusInput || !strings.Contains(h.screen(), "Tab to answer") {
		t.Fatalf("Esc should return to the input with the question still shown:\n%s", h.screen())
	}
	h.press(tea.KeyTab, 0)
	h.typeText("d")
	if len(api.answers) != 1 || api.answers[0].Decision != "decline" || api.answers[0].Choice != "" {
		t.Fatalf("answers = %+v", api.answers)
	}
}

func TestCtrlCInterruptsTheRunningTurnThenQuits(t *testing.T) {
	api := newFake()
	h := start(t, api)
	h.typeText("long job")
	h.press(tea.KeyEnter, 0)
	turn := "t" + api.turns[0].CommandID
	h.press('c', tea.ModCtrl)
	if h.quit || len(api.interrupts) != 1 || !strings.HasPrefix(api.interrupts[0], turn+"/") {
		t.Fatalf("first Ctrl-C: quit=%v interrupts=%v", h.quit, api.interrupts)
	}
	h.press('c', tea.ModCtrl)
	if !h.quit || len(api.interrupts) != 1 {
		t.Fatalf("second Ctrl-C: quit=%v interrupts=%v", h.quit, api.interrupts)
	}
}

func TestCtrlCWithNothingRunningAsksBeforeQuitting(t *testing.T) {
	h := start(t, newFake())
	h.press('c', tea.ModCtrl)
	if h.quit || !strings.Contains(h.screen(), "Ctrl-C again") {
		t.Fatalf("first Ctrl-C quit or said nothing:\n%s", h.screen())
	}
	h.typeText("a")
	h.press('c', tea.ModCtrl)
	if h.quit {
		t.Fatal("a key between presses should disarm the quit")
	}
	h.press('c', tea.ModCtrl)
	if !h.quit {
		t.Fatal("second Ctrl-C did not quit")
	}
	h2 := start(t, newFake())
	h2.press('d', tea.ModCtrl)
	if !h2.quit {
		t.Fatal("Ctrl-D did not quit")
	}
}

// After the stream drops, a reconnect re-reads the thread from the server
// and shows each item once, including the ones recorded while away.
func TestReconnectRereadsWithoutDuplicatingItems(t *testing.T) {
	api := newFake()
	h := start(t, api)
	h.send(StreamMsg{Link: clientsdk.Connected})
	h.typeText("before restart")
	h.press(tea.KeyEnter, 0)
	turn := "t" + api.turns[0].CommandID
	h.event(clientapi.Event{Type: clientapi.EventTurnProgress, Thread: "console:main", Turn: turn, Progress: &clientapi.Progress{Answer: "half"}})
	h.send(StreamMsg{Link: clientsdk.Disconnected, Err: errors.New("EOF")})
	if !strings.Contains(h.screen(), "reconnecting") {
		t.Fatalf("header does not show the lost connection:\n%s", h.screen())
	}
	// The server restarted and finished the turn while the client was away.
	api.finish(turn, "done after restart")
	reads := api.reads
	h.send(StreamMsg{Link: clientsdk.Connected})
	h.send(StreamMsg{Link: clientsdk.Connected, Reset: true})
	if api.reads <= reads {
		t.Fatal("reconnecting did not re-read the thread")
	}
	screen := h.screen()
	if strings.Count(screen, "before restart") != 1 || strings.Count(screen, "done after restart") != 1 || strings.Contains(screen, "half") {
		t.Fatalf("after the reconnect each item should show once, without stale progress:\n%s", screen)
	}
	if !strings.Contains(screen, "connected") || strings.Contains(screen, "reconnecting") {
		t.Fatalf("header after the reconnect:\n%s", screen)
	}
}

func TestThreadPickerSwitchesThreads(t *testing.T) {
	api := newFake()
	h := start(t, api)
	h.press('t', tea.ModCtrl)
	if !strings.Contains(h.screen(), "+ New thread") || !strings.Contains(h.screen(), "console:main") {
		t.Fatalf("picker:\n%s", h.screen())
	}
	h.press(tea.KeyUp, 0)
	h.press(tea.KeyEnter, 0)
	if !strings.HasPrefix(h.model.thread.ID, "console:tui-") || h.model.thread.Project != "scratch" || h.model.focus != focusInput {
		t.Fatalf("new thread = %+v", h.model.thread)
	}
}

// A send whose answer is lost belongs to its thread: it is not restored
// into another thread's input, and retrying it in its own thread repeats
// the original request.
func TestLostSendStaysWithItsThread(t *testing.T) {
	api := newFake()
	h := start(t, api)
	api.failStart = errors.New("connection reset")
	h.typeText("deploy")
	// The answer to thread A's send is held while the user moves on.
	next, held := h.model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	h.model = next.(Model)
	h.press('t', tea.ModCtrl)
	h.press(tea.KeyUp, 0)
	h.press(tea.KeyEnter, 0)
	other := h.model.thread.ID
	if other == "console:main" {
		t.Fatal("did not switch threads")
	}
	h.run(held)
	original := api.turns[0]
	if h.model.input.Value() != "" || strings.Contains(h.screen(), "not sent") {
		t.Fatalf("thread A's lost send leaked into thread B: %q\n%s", h.model.input.Value(), h.screen())
	}
	api.failStart = nil
	h.typeText("deploy")
	h.press(tea.KeyEnter, 0)
	last := api.turns[len(api.turns)-1]
	if last.Thread != other || last.CommandID == original.CommandID {
		t.Fatalf("a send in thread B reused thread A's request: %+v", last)
	}
	// Back in thread A the unsent input is offered again, and Enter
	// repeats the original request.
	h.model.switchToNow(t, h, "console:main")
	if h.model.input.Value() != "deploy" {
		t.Fatalf("thread A's input = %q", h.model.input.Value())
	}
	h.press(tea.KeyEnter, 0)
	if last := api.turns[len(api.turns)-1]; !reflect.DeepEqual(last, original) {
		t.Fatalf("retry = %+v, want %+v", last, original)
	}
}

// switchToNow opens thread id through the picker.
func (m Model) switchToNow(t *testing.T, h *harness, id string) {
	t.Helper()
	h.press('t', tea.ModCtrl)
	for i, th := range h.model.picker.threads {
		if th.ID == id {
			h.model.picker.selected = i + 1
		}
	}
	h.press(tea.KeyEnter, 0)
	if h.model.thread.ID != id {
		t.Fatalf("thread = %q, want %q", h.model.thread.ID, id)
	}
}

// Two sends in flight whose answers are both lost keep their own
// command_ids: each retry repeats its own request.
func TestTwoLostSendsRetryWithTheirOwnCommandIDs(t *testing.T) {
	api := newFake()
	h := start(t, api)
	api.failStart = errors.New("connection reset")
	var held []tea.Cmd
	for _, text := range []string{"first", "second"} {
		h.typeText(text)
		next, cmd := h.model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		h.model = next.(Model)
		held = append(held, cmd)
	}
	for _, cmd := range held {
		h.run(cmd)
	}
	if len(api.turns) != 2 {
		t.Fatalf("turns = %+v", api.turns)
	}
	sentFirst, sentSecond := api.turns[0], api.turns[1]
	api.failStart = nil
	if h.model.input.Value() != "first" {
		t.Fatalf("input = %q, want the oldest unsent input", h.model.input.Value())
	}
	h.press(tea.KeyEnter, 0)
	if h.model.input.Value() != "second" {
		t.Fatalf("after retrying the first, input = %q", h.model.input.Value())
	}
	h.press(tea.KeyEnter, 0)
	if len(api.turns) != 4 || !reflect.DeepEqual(api.turns[2], sentFirst) || !reflect.DeepEqual(api.turns[3], sentSecond) {
		t.Fatalf("retries = %+v, want %+v then %+v", api.turns[2:], sentFirst, sentSecond)
	}
}

// Opening an existing thread from the picker does not crash.
func TestThreadPickerOpensAnExistingThread(t *testing.T) {
	api := newFake()
	api.others = map[string]clientapi.ThreadDetail{"console:other": {Thread: clientapi.Thread{ID: "console:other", Transport: "console", Project: "scratch"},
		Items: []clientapi.Item{{ID: "u1", Kind: clientapi.ItemMessage, Role: "user", Text: "from the other thread"}}}}
	h := start(t, api)
	h.press('t', tea.ModCtrl)
	// The current thread is preselected: Enter reopens it.
	h.press(tea.KeyEnter, 0)
	if h.model.thread.ID != "console:main" || h.model.focus != focusInput {
		t.Fatalf("reopened thread = %+v", h.model.thread)
	}
	h.model.switchToNow(t, h, "console:other")
	if !strings.Contains(h.screen(), "from the other thread") {
		t.Fatalf("the other thread is not shown:\n%s", h.screen())
	}
}

// A question that only takes text can still be declined or cancelled:
// the first Esc leaves the answer field.
func TestFreeTextQuestionCanBeDeclinedOrCancelled(t *testing.T) {
	for _, tc := range []struct{ key, decision string }{{"d", "decline"}, {"c", "cancel"}} {
		api := newFake()
		api.detail.Items = []clientapi.Item{{ID: "question:q2", Kind: clientapi.ItemQuestion, Thread: "console:main", Question: &clientapi.Question{
			ID: "q2", Thread: "console:main", Message: "Name the branch", State: questionPending, AllowFreeText: true}}}
		h := start(t, api)
		if !h.model.question.writing || h.model.focus != focusQuestion {
			t.Fatalf("a text-only question should open its answer field: %+v", h.model.question)
		}
		h.press(tea.KeyEscape, 0)
		if h.model.question.writing || h.model.focus != focusQuestion {
			t.Fatalf("the first Esc should leave the answer field and keep the question: writing=%v focus=%v", h.model.question.writing, h.model.focus)
		}
		h.typeText(tc.key)
		if len(api.answers) != 1 || api.answers[0].Decision != tc.decision || api.answers[0].Text != "" {
			t.Fatalf("%s: answers = %+v", tc.key, api.answers)
		}
	}
	// A second Esc puts the question off.
	api := newFake()
	api.detail.Items = []clientapi.Item{{ID: "question:q2", Kind: clientapi.ItemQuestion, Thread: "console:main", Question: &clientapi.Question{
		ID: "q2", Thread: "console:main", Message: "Name the branch", State: questionPending, AllowFreeText: true}}}
	h := start(t, api)
	h.press(tea.KeyEscape, 0)
	h.press(tea.KeyEscape, 0)
	if h.model.focus != focusInput || len(api.answers) != 0 {
		t.Fatalf("second Esc: focus=%v answers=%+v", h.model.focus, api.answers)
	}
}

// A bootstrap that fails is retried, after its pause and on the next
// connection, and the error goes once it succeeds.
func TestFailedBootstrapIsRetried(t *testing.T) {
	api := newFake()
	api.failProjects = 2
	h := start(t, api)
	if h.model.loaded || !strings.Contains(h.screen(), "could not open a thread") {
		t.Fatalf("the failure should be shown:\n%s", h.screen())
	}
	// The pause elapsed; the server is still unreachable.
	h.send(bootRetryMsg{})
	if h.model.loaded || h.model.booting {
		t.Fatalf("second attempt: loaded=%v booting=%v", h.model.loaded, h.model.booting)
	}
	// The stream reconnects: the client boots without waiting.
	h.send(StreamMsg{Link: clientsdk.Connected})
	if !h.model.loaded || strings.Contains(h.screen(), "could not open") {
		t.Fatalf("the client did not recover:\n%s", h.screen())
	}
	h.typeText("hello")
	h.press(tea.KeyEnter, 0)
	if len(api.turns) != 1 {
		t.Fatalf("sending after recovery: %+v", api.turns)
	}
}

// A bootstrap attempt is not started while one is running.
func TestBootstrapAttemptsDoNotOverlap(t *testing.T) {
	m := New(t.Context(), newFake(), Options{})
	next, cmd := m.Update(bootRetryMsg{})
	if cmd != nil {
		t.Fatal("a retry started a second bootstrap while the first was running")
	}
	next, cmd = next.(Model).Update(StreamMsg{Link: clientsdk.Connected})
	if cmd != nil || !next.(Model).booting {
		t.Fatal("a connection started a second bootstrap while the first was running")
	}
}

// An event that arrives while the bootstrap reads is not lost: the thread
// is read again once the first snapshot is installed.
func TestEventsDuringBootstrapAreNotLost(t *testing.T) {
	api := newFake()
	h := &harness{t: t, model: New(t.Context(), api, Options{})}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 50})
	boot := collect(h.model.Init())
	// The server records a question after the snapshot was read; its event
	// reaches the client before the snapshot does.
	api.mu.Lock()
	api.detail.Items = append(api.detail.Items, pendingQuestion())
	api.mu.Unlock()
	h.event(clientapi.Event{Type: clientapi.EventQuestionUpdated, Thread: "console:main", Question: "q1"})
	for _, msg := range boot {
		h.send(msg)
	}
	if !strings.Contains(h.screen(), "Run a command") {
		t.Fatalf("the question recorded during bootstrap is missing:\n%s", h.screen())
	}
}

// A failed read is shown until a read succeeds, without erasing a send
// error that is still true.
func TestRecoveredReadClearsItsError(t *testing.T) {
	api := newFake()
	h := start(t, api)
	api.failReads = 1
	h.event(clientapi.Event{Type: clientapi.EventThreadUpdated, Thread: "console:main"})
	if !strings.Contains(h.screen(), "could not read the thread") {
		t.Fatalf("read error not shown:\n%s", h.screen())
	}
	h.event(clientapi.Event{Type: clientapi.EventThreadUpdated, Thread: "console:main"})
	if strings.Contains(h.screen(), "could not read the thread") {
		t.Fatalf("a successful read left the error:\n%s", h.screen())
	}
	api.failStart = errors.New("connection refused")
	h.typeText("deploy")
	h.press(tea.KeyEnter, 0)
	h.event(clientapi.Event{Type: clientapi.EventThreadUpdated, Thread: "console:main"})
	if !strings.Contains(h.screen(), "not sent") {
		t.Fatalf("a successful read erased the send error:\n%s", h.screen())
	}
}

// A delegated step that finishes after its parent turn ended updates the
// recorded reply: the client re-reads the thread.
func TestStepProgressForARecordedReplyRefreshesItems(t *testing.T) {
	api := newFake()
	api.detail.Turns = []clientapi.Turn{{ID: "t1", State: clientapi.TurnDone, ReplyItem: "r1"}}
	step := func(status string) []clientapi.Item {
		return []clientapi.Item{
			{ID: "r1", Kind: clientapi.ItemMessage, Role: "assistant", Text: "Delegated.", Turn: "t1"},
			{ID: "r1#step-s1", Kind: clientapi.ItemToolCall, Turn: "t1", Tool: &clientapi.ToolCall{ID: "s1", Kind: "step", Name: "run the tests", Status: status}},
		}
	}
	api.detail.Items = step("running")
	h := start(t, api)
	if !strings.Contains(h.screen(), "… run the tests") {
		t.Fatalf("step not shown:\n%s", h.screen())
	}
	api.mu.Lock()
	api.detail.Items = step("completed")
	api.mu.Unlock()
	h.event(clientapi.Event{Type: clientapi.EventTurnProgress, Thread: "console:main", Turn: "t1", Item: "r1", Task: "s1", Progress: &clientapi.Progress{Phase: "completed"}})
	if !strings.Contains(h.screen(), "✓ run the tests") {
		t.Fatalf("the finished step is not shown:\n%s", h.screen())
	}
}

// A question larger than the terminal is shown in a bounded, scrollable
// region that keeps the selected option and the controls on screen.
func TestLargeQuestionFitsTheTerminal(t *testing.T) {
	api := newFake()
	q := pendingQuestion()
	q.Question.Message = strings.Repeat("A long explanation line.\n", 40)
	q.Question.Options = nil
	for i := range 64 {
		q.Question.Options = append(q.Question.Options, clientapi.QuestionOption{ID: fmt.Sprint(i), Label: fmt.Sprintf("Option %02d", i)})
	}
	api.detail.Items = []clientapi.Item{q}
	h := start(t, api)
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	check := func(want string) {
		t.Helper()
		screen := h.screen()
		if n := strings.Count(screen, "\n") + 1; n > 24 {
			t.Fatalf("the screen is %d rows on a 24-row terminal:\n%s", n, screen)
		}
		if !strings.Contains(screen, "› "+want) || !strings.Contains(screen, "decline") || !strings.Contains(screen, "cancel") {
			t.Fatalf("the selection %q or the controls are off screen:\n%s", want, screen)
		}
	}
	check("Option 00")
	for range 63 {
		h.press(tea.KeyDown, 0)
	}
	check("Option 63")
	h.press(tea.KeyEnter, 0)
	if len(api.answers) != 1 || api.answers[0].Choice != "63" {
		t.Fatalf("answers = %+v", api.answers)
	}
}

// PgUp scrolls the question to its message.
func TestQuestionScrollsWithPaging(t *testing.T) {
	api := newFake()
	q := pendingQuestion()
	q.Question.Message = "TOP OF MESSAGE\n" + strings.Repeat("filler\n", 30)
	api.detail.Items = []clientapi.Item{q}
	h := start(t, api)
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	if strings.Contains(h.screen(), "│ TOP OF MESSAGE") || !strings.Contains(h.screen(), "› Allow") {
		t.Fatalf("the selected option should be scrolled into view first:\n%s", h.screen())
	}
	for range 10 {
		h.press(tea.KeyPgUp, 0)
	}
	if !strings.Contains(h.screen(), "│ TOP OF MESSAGE") {
		t.Fatalf("PgUp did not reach the top of the question:\n%s", h.screen())
	}
}
