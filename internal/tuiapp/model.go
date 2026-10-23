// Package tuiapp is Steve's interactive terminal client. It speaks
// steve.client.v1 through clientsdk and keeps no state the server does not
// hold: every transcript it shows is a page the server returned, re-read
// whenever an event says the thread moved.
package tuiapp

import (
	"context"
	"errors"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/gopact-ai/steve/internal/clientapi"
	"github.com/gopact-ai/steve/internal/clientsdk"
)

// API is the part of the protocol the terminal client uses.
// *clientsdk.Client implements it.
type API interface {
	Initialize(context.Context) (clientapi.Initialize, error)
	Threads(context.Context) ([]clientapi.Thread, error)
	Thread(context.Context, string, clientsdk.Page) (clientapi.ThreadDetail, error)
	CreateThread(context.Context, clientapi.CreateThread) (clientapi.ThreadDetail, error)
	StartTurn(context.Context, clientapi.StartTurn) (clientapi.Turn, error)
	Interrupt(context.Context, string, string) (clientapi.InterruptResult, error)
	Answer(context.Context, string, clientapi.QuestionAnswer) (clientapi.AnswerResult, error)
	Projects(context.Context) ([]clientapi.Project, error)
	Agents(context.Context) ([]clientapi.Agent, error)
	Follow(context.Context, string, string) <-chan clientsdk.Update
}

// Options choose what the client opens.
type Options struct {
	// Thread is opened, or created in Project when it does not exist.
	// Empty opens the most recent console thread.
	Thread string
	// Project binds a thread the client creates; empty is the server's
	// default project.
	Project string
	// Notice is shown in the status line at start.
	Notice string
}

// pageSize is how many of a thread's newest items are shown.
const pageSize = 200

// questionPending is the state of a question still waiting for a decision.
const questionPending = "pending"

type focus int

const (
	focusInput focus = iota
	focusQuestion
	focusPicker
)

// Model is the Bubble Tea model of the terminal client.
type Model struct {
	ctx  context.Context
	api  API
	opts Options

	width, height int
	input         textarea.Model
	transcript    viewport.Model
	follow        bool
	focus         focus

	server   clientapi.Initialize
	projects []clientapi.Project
	agents   []clientapi.Agent
	thread   clientapi.Thread
	turns    []clientapi.Turn
	items    []clientapi.Item
	loaded   bool

	link clientsdk.Link
	// refreshing is set while a thread read is in flight; dirty asks for
	// another once it returns, so a burst of events costs two reads.
	refreshing, dirty bool

	progress   map[string]*clientapi.Progress
	steps      map[string]map[string]*clientapi.Progress
	outbox     []pendingSend
	failedSend *pendingSend
	interrupts map[string]*pendingInterrupt
	answers    map[string]pendingAnswer
	question   questionState
	picker     pickerState

	quitArmed bool
	status    string
	err       string
}

type pendingSend struct {
	input, commandID string
}

type pendingInterrupt struct {
	commandID string
	accepted  bool
}

type pendingAnswer struct {
	answer clientapi.QuestionAnswer
	failed bool
}

type questionState struct {
	id       string
	selected int
	writing  bool
	text     textinput.Model
}

type pickerState struct {
	threads  []clientapi.Thread
	selected int
	loading  bool
}

// StreamMsg delivers one update of the followed event stream. Run pumps
// them from clientsdk.Client.Follow.
type StreamMsg clientsdk.Update

type bootMsg struct {
	server   clientapi.Initialize
	projects []clientapi.Project
	agents   []clientapi.Agent
	detail   clientapi.ThreadDetail
	err      error
}

type detailMsg struct {
	thread string
	detail clientapi.ThreadDetail
	err    error
	// opened is a thread the client just created and switches to.
	opened bool
}

type sentMsg struct {
	send pendingSend
	turn clientapi.Turn
	err  error
}

type interruptedMsg struct {
	turn   string
	result clientapi.InterruptResult
	err    error
}

type answeredMsg struct {
	question string
	result   clientapi.AnswerResult
	err      error
}

type threadsMsg struct {
	threads []clientapi.Thread
	err     error
}

// New returns the client model. ctx bounds every request it makes.
func New(ctx context.Context, api API, opts Options) Model {
	input := textarea.New()
	input.ShowLineNumbers = false
	input.Prompt = "› "
	input.Placeholder = "Message Steve, or /help for commands"
	input.SetHeight(3)
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	input.Focus()
	text := textinput.New()
	text.Prompt = "answer › "
	return Model{
		ctx: ctx, api: api, opts: opts, input: input, transcript: viewport.New(), follow: true,
		progress: map[string]*clientapi.Progress{}, steps: map[string]map[string]*clientapi.Progress{},
		interrupts: map[string]*pendingInterrupt{}, answers: map[string]pendingAnswer{},
		question: questionState{text: text}, status: opts.Notice, width: 80, height: 24,
	}
}

func (m Model) Init() tea.Cmd { return m.boot() }

// boot reads what the header needs and opens the first thread.
func (m Model) boot() tea.Cmd {
	ctx, api, opts := m.ctx, m.api, m.opts
	return func() tea.Msg {
		var out bootMsg
		if out.server, out.err = api.Initialize(ctx); out.err != nil {
			return out
		}
		if out.projects, out.err = api.Projects(ctx); out.err != nil {
			return out
		}
		if out.agents, out.err = api.Agents(ctx); out.err != nil {
			return out
		}
		out.detail, out.err = openThread(ctx, api, opts, out.projects)
		return out
	}
}

// openThread reads the thread the options name, creating it when absent,
// or the most recent console thread, creating console:main when there is
// none.
func openThread(ctx context.Context, api API, opts Options, projects []clientapi.Project) (clientapi.ThreadDetail, error) {
	id := opts.Thread
	if id == "" {
		threads, err := api.Threads(ctx)
		if err != nil {
			return clientapi.ThreadDetail{}, err
		}
		for _, t := range threads {
			if !t.ReadOnly && !t.Archived {
				return api.Thread(ctx, t.ID, clientsdk.Page{Limit: pageSize})
			}
		}
		id = "main"
	} else {
		detail, err := api.Thread(ctx, id, clientsdk.Page{Limit: pageSize})
		if !clientsdk.IsCode(err, clientapi.CodeNotFound) {
			return detail, err
		}
	}
	project := opts.Project
	if project == "" {
		project = defaultProject(projects)
	}
	if project == "" {
		return clientapi.ThreadDetail{}, errors.New("the server declares no project to open a thread in")
	}
	return api.CreateThread(ctx, clientapi.CreateThread{ID: id, Project: project})
}

func defaultProject(projects []clientapi.Project) string {
	for _, p := range projects {
		if p.Default {
			return p.ID
		}
	}
	if len(projects) > 0 {
		return projects[0].ID
	}
	return ""
}

// Update applies one message, then lays the screen out again.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	next.layout()
	return next, cmd
}

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.PasteMsg:
		return m.typed(msg)
	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.transcript, cmd = m.transcript.Update(msg)
		m.follow = m.transcript.AtBottom()
		return m, cmd
	case bootMsg:
		return m.booted(msg)
	case StreamMsg:
		return m.stream(clientsdk.Update(msg))
	case detailMsg:
		return m.detailed(msg)
	case sentMsg:
		return m.sent(msg)
	case interruptedMsg:
		return m.interrupted(msg)
	case answeredMsg:
		return m.answered(msg)
	case threadsMsg:
		return m.listed(msg)
	}
	return m.typed(msg)
}

func (m Model) booted(msg bootMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.err = "could not open a thread: " + msg.err.Error()
		return m, nil
	}
	m.server, m.projects, m.agents = msg.server, msg.projects, msg.agents
	m.show(msg.detail)
	return m, nil
}

// show replaces what the client displays of the current thread with a
// page the server returned. Items are never merged into an older copy, so
// a re-read after a reconnect cannot duplicate one.
func (m *Model) show(detail clientapi.ThreadDetail) {
	m.thread, m.turns, m.items, m.loaded = detail.Thread, detail.Turns, detail.Items, true
	running := map[string]bool{}
	for _, t := range m.turns {
		if t.State == clientapi.TurnRunning {
			running[t.ID] = true
		}
	}
	for id := range m.progress {
		if !running[id] {
			delete(m.progress, id)
		}
	}
	for id := range m.steps {
		if !running[id] {
			delete(m.steps, id)
		}
	}
	m.pickQuestion()
}

// refresh re-reads the current thread, or asks for another read when one
// is already in flight.
func (m *Model) refresh() tea.Cmd {
	if !m.loaded || m.thread.ID == "" {
		return nil
	}
	if m.refreshing {
		m.dirty = true
		return nil
	}
	m.refreshing = true
	ctx, api, id := m.ctx, m.api, m.thread.ID
	return func() tea.Msg {
		detail, err := api.Thread(ctx, id, clientsdk.Page{Limit: pageSize})
		return detailMsg{thread: id, detail: detail, err: err}
	}
}

func (m Model) detailed(msg detailMsg) (Model, tea.Cmd) {
	m.refreshing = false
	if msg.opened {
		if msg.err != nil {
			m.err = "could not create a thread: " + msg.err.Error()
			return m, nil
		}
		clear(m.progress)
		clear(m.steps)
		m.follow, m.err, m.status = true, "", ""
		m.show(msg.detail)
		return m, nil
	}
	if msg.thread != m.thread.ID {
		return m, m.refresh()
	}
	if msg.err != nil {
		m.err = "could not read the thread: " + msg.err.Error()
	} else {
		m.show(msg.detail)
	}
	if m.dirty {
		m.dirty = false
		return m, m.refresh()
	}
	return m, nil
}

// stream applies one update of the event stream. A new connection or a
// reset re-reads the thread, which is how the client resumes after a
// restart without trusting what it showed before.
func (m Model) stream(u clientsdk.Update) (Model, tea.Cmd) {
	if u.Link == clientsdk.Disconnected {
		m.link = u.Link
		if u.Err != nil {
			m.status = "connection lost: " + u.Err.Error()
		}
		return m, nil
	}
	if m.link == clientsdk.Disconnected {
		// Clear the lost-connection message; a start notice stays.
		m.status = ""
	}
	m.link = u.Link
	if u.Event == nil {
		if u.Reset {
			clear(m.progress)
			clear(m.steps)
		}
		return m, m.refresh()
	}
	ev := u.Event
	if ev.Thread != m.thread.ID {
		return m, nil
	}
	switch ev.Type {
	case clientapi.EventTurnProgress:
		m.progressed(ev)
		return m, nil
	case clientapi.EventThreadDeleted:
		m.err = "this thread was deleted; press Ctrl-T to open another"
		return m, nil
	case clientapi.EventItemCompleted, clientapi.EventItemRemoved, clientapi.EventQuestionUpdated, clientapi.EventTurnUpdated, clientapi.EventThreadUpdated:
		return m, m.refresh()
	}
	return m, nil
}

func (m *Model) progressed(ev *clientapi.Event) {
	if ev.Progress == nil || ev.Turn == "" {
		return
	}
	if ev.Item == "" {
		m.progress[ev.Turn] = ev.Progress
		return
	}
	if m.steps[ev.Turn] == nil {
		m.steps[ev.Turn] = map[string]*clientapi.Progress{}
	}
	m.steps[ev.Turn][ev.Item] = ev.Progress
}

// runningTurn is the turn of the current thread that is running, if any.
func (m Model) runningTurn() *clientapi.Turn {
	for i := range m.turns {
		if m.turns[i].State == clientapi.TurnRunning {
			return &m.turns[i]
		}
	}
	return nil
}

func (m Model) queuedTurns() int {
	n := 0
	for _, t := range m.turns {
		if t.State == clientapi.TurnQueued {
			n++
		}
	}
	return n
}

// pendingQuestions are the questions of the current thread still waiting.
func (m Model) pendingQuestions() []*clientapi.Question {
	var out []*clientapi.Question
	for _, item := range m.items {
		if item.Kind == clientapi.ItemQuestion && item.Question != nil && item.Question.State == questionPending {
			out = append(out, item.Question)
		}
	}
	return out
}

// pickQuestion keeps the prompt on the question it shows while that one
// is pending, and moves it to the next one otherwise. A newly shown
// question takes the keyboard.
func (m *Model) pickQuestion() {
	pending := m.pendingQuestions()
	for _, q := range pending {
		if q.ID == m.question.id {
			return
		}
	}
	m.question.id, m.question.selected, m.question.writing = "", 0, false
	m.question.text.SetValue("")
	m.question.text.Blur()
	if len(pending) == 0 {
		if m.focus == focusQuestion {
			m.focusInput()
		}
		return
	}
	m.question.id = pending[0].ID
	if len(pending[0].Options) == 0 && pending[0].AllowFreeText {
		m.question.writing = true
		m.question.text.Focus()
	}
	if m.focus != focusPicker {
		m.focus = focusQuestion
		m.input.Blur()
	}
}

func (m Model) currentQuestion() *clientapi.Question {
	for _, q := range m.pendingQuestions() {
		if q.ID == m.question.id {
			return q
		}
	}
	return nil
}

func (m *Model) focusInput() {
	m.focus = focusInput
	m.input.Focus()
}

func trimmed(s string) string { return strings.TrimSpace(s) }
