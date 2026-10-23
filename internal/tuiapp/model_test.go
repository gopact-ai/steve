package tuiapp

import (
	"context"
	"errors"
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
}

func newFake() *fakeAPI {
	return &fakeAPI{detail: clientapi.ThreadDetail{Thread: clientapi.Thread{ID: "console:main", Transport: "console", Project: "scratch", Agent: "codex"}}}
}

func (f *fakeAPI) Initialize(context.Context) (clientapi.Initialize, error) {
	return clientapi.Initialize{Protocol: clientapi.Protocol}, nil
}
func (f *fakeAPI) Projects(context.Context) ([]clientapi.Project, error) {
	return []clientapi.Project{{ID: "scratch", Default: true}}, nil
}
func (f *fakeAPI) Agents(context.Context) ([]clientapi.Agent, error) {
	return []clientapi.Agent{{ID: "codex", Node: "laptop", Default: true}}, nil
}
func (f *fakeAPI) Threads(context.Context) ([]clientapi.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []clientapi.Thread{f.detail.Thread}, nil
}
func (f *fakeAPI) Thread(_ context.Context, id string, _ clientsdk.Page) (clientapi.ThreadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if id != f.detail.Thread.ID {
		return clientapi.ThreadDetail{}, &clientsdk.Error{Status: 404, Code: clientapi.CodeNotFound}
	}
	d := f.detail
	d.Items = append([]clientapi.Item(nil), f.detail.Items...)
	d.Turns = append([]clientapi.Turn(nil), f.detail.Turns...)
	return d, nil
}
func (f *fakeAPI) CreateThread(_ context.Context, req clientapi.CreateThread) (clientapi.ThreadDetail, error) {
	return clientapi.ThreadDetail{Thread: clientapi.Thread{ID: "console:" + req.ID, Transport: "console", Project: req.Project}}, nil
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
		case bootMsg, detailMsg, sentMsg, interruptedMsg, answeredMsg, threadsMsg:
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
