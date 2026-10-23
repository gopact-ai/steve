package tuiapp

import (
	tea "charm.land/bubbletea/v2"

	"github.com/gopact-ai/steve/internal/clientapi"
	"github.com/gopact-ai/steve/internal/clientsdk"
)

// key handles a key press. Ctrl-C, Ctrl-D and Ctrl-T work everywhere; the
// rest goes to whatever has the keyboard.
func (m Model) key(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	press := msg.String()
	if press != "ctrl+c" {
		m.quitArmed = false
	}
	switch press {
	case "ctrl+c":
		return m.ctrlC()
	case "ctrl+d":
		return m, tea.Quit
	case "ctrl+t":
		return m.openPicker()
	case "pgup", "pgdown":
		if press == "pgup" {
			m.transcript.PageUp()
		} else {
			m.transcript.PageDown()
		}
		m.follow = m.transcript.AtBottom()
		return m, nil
	}
	switch m.focus {
	case focusPicker:
		return m.pickerKey(press)
	case focusQuestion:
		return m.questionKey(msg)
	}
	if press == "enter" {
		return m.send()
	}
	if press == "tab" && m.currentQuestion() != nil {
		m.focus = focusQuestion
		m.input.Blur()
		if m.question.writing {
			m.question.text.Focus()
		}
		return m, nil
	}
	return m.typed(msg)
}

// typed passes a message to the field that has the keyboard.
func (m Model) typed(msg tea.Msg) (Model, tea.Cmd) {
	var cmd tea.Cmd
	switch {
	case m.focus == focusQuestion && m.question.writing:
		m.question.text, cmd = m.question.text.Update(msg)
	case m.focus == focusInput:
		m.input, cmd = m.input.Update(msg)
	}
	return m, cmd
}

// ctrlC stops the running turn; a second press quits. With nothing
// running the first press only arms the quit.
func (m Model) ctrlC() (Model, tea.Cmd) {
	if m.quitArmed {
		return m, tea.Quit
	}
	m.quitArmed = true
	running := m.runningTurn()
	if running == nil {
		m.status = "Press Ctrl-C again to quit"
		return m, nil
	}
	pending := m.interrupts[running.ID]
	if pending != nil && pending.accepted {
		m.status = "Stopping the turn; press Ctrl-C again to quit"
		return m, nil
	}
	if pending == nil {
		// The key is kept until the server accepts it, so a retry after a
		// lost answer names the same stop.
		pending = &pendingInterrupt{commandID: clientsdk.NewCommandID("tui-stop-")}
		m.interrupts[running.ID] = pending
	}
	m.status = "Interrupting; press Ctrl-C again to quit"
	ctx, api, turn, id := m.ctx, m.api, running.ID, pending.commandID
	return m, func() tea.Msg {
		result, err := api.Interrupt(ctx, turn, id)
		return interruptedMsg{turn: turn, result: result, err: err}
	}
}

func (m Model) interrupted(msg interruptedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		if clientsdk.IsCode(msg.err, clientapi.CodeConflict) {
			delete(m.interrupts, msg.turn)
			m.status = "The turn had already ended"
			return m, m.refresh()
		}
		m.err = "interrupt failed: " + msg.err.Error()
		return m, nil
	}
	if pending := m.interrupts[msg.turn]; pending != nil {
		pending.accepted = true
	}
	m.status = "Interrupt sent"
	return m, m.refresh()
}

// send submits the input box as a turn. Input starting with / is a Steve
// command and goes to the server like any other input. A send that failed
// is retried with its command_id, so it cannot run twice.
func (m Model) send() (Model, tea.Cmd) {
	text := trimmed(m.input.Value())
	if text == "" {
		return m, nil
	}
	if !m.loaded {
		m.err = "not connected to a thread yet"
		return m, nil
	}
	if m.thread.ReadOnly {
		m.err = "this thread belongs to a chat channel and is read-only; press Ctrl-T to open a console thread"
		return m, nil
	}
	send := pendingSend{input: text, commandID: clientsdk.NewCommandID("tui-")}
	if m.failedSend != nil && m.failedSend.input == text {
		send.commandID = m.failedSend.commandID
	}
	m.failedSend, m.err, m.status = nil, "", ""
	m.outbox = append(m.outbox, send)
	m.input.Reset()
	m.follow = true
	ctx, api := m.ctx, m.api
	req := clientapi.StartTurn{Thread: m.thread.ID, Input: text, CommandID: send.commandID, ExpectedProject: m.thread.Project}
	return m, func() tea.Msg {
		turn, err := api.StartTurn(ctx, req)
		return sentMsg{send: send, turn: turn, err: err}
	}
}

func (m Model) sent(msg sentMsg) (Model, tea.Cmd) {
	for i, s := range m.outbox {
		if s.commandID == msg.send.commandID {
			m.outbox = append(m.outbox[:i:i], m.outbox[i+1:]...)
			break
		}
	}
	if msg.err != nil {
		failed := msg.send
		m.failedSend = &failed
		if trimmed(m.input.Value()) == "" {
			m.input.SetValue(msg.send.input)
		}
		m.err = "not sent: " + msg.err.Error() + " (Enter retries)"
		return m, nil
	}
	return m, m.refresh()
}

// questionKey answers the question on screen. Enter takes the selected
// option (or the written answer), d declines, c cancels, Esc returns to
// the input box with the question still waiting.
func (m Model) questionKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	q := m.currentQuestion()
	if q == nil {
		m.focusInput()
		return m, nil
	}
	press := msg.String()
	if press == "esc" {
		if m.question.writing && len(q.Options) > 0 {
			m.question.writing = false
			m.question.text.Blur()
			return m, nil
		}
		m.focusInput()
		return m, nil
	}
	if m.question.writing {
		if press == "enter" {
			if text := trimmed(m.question.text.Value()); text != "" {
				return m.answer(q, clientapi.QuestionAnswer{Decision: "accept", Text: text})
			}
			return m, nil
		}
		return m.typed(msg)
	}
	choices := len(q.Options)
	if q.AllowFreeText {
		choices++
	}
	switch press {
	case "up", "k":
		m.question.selected = (m.question.selected + choices - 1) % max(choices, 1)
	case "down", "j":
		m.question.selected = (m.question.selected + 1) % max(choices, 1)
	case "enter":
		if m.question.selected < len(q.Options) {
			return m.answer(q, clientapi.QuestionAnswer{Decision: "accept", Choice: q.Options[m.question.selected].ID})
		}
		if q.AllowFreeText {
			m.question.writing = true
			return m, m.question.text.Focus()
		}
	case "d":
		return m.answer(q, clientapi.QuestionAnswer{Decision: "decline"})
	case "c":
		return m.answer(q, clientapi.QuestionAnswer{Decision: "cancel"})
	}
	return m, nil
}

// answer sends a decision. The same decision retried after a failure
// keeps its command_id, so the server answers it from its receipt.
func (m Model) answer(q *clientapi.Question, answer clientapi.QuestionAnswer) (Model, tea.Cmd) {
	last, ok := m.answers[q.ID]
	same := ok && last.answer.Decision == answer.Decision && last.answer.Choice == answer.Choice && last.answer.Text == answer.Text
	if same {
		answer.CommandID = last.answer.CommandID
	} else {
		answer.CommandID = clientsdk.NewCommandID("tui-answer-")
	}
	m.answers[q.ID] = pendingAnswer{answer: answer}
	m.err = ""
	ctx, api, id := m.ctx, m.api, q.ID
	return m, func() tea.Msg {
		result, err := api.Answer(ctx, id, answer)
		return answeredMsg{question: id, result: result, err: err}
	}
}

func (m Model) answered(msg answeredMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		if last, ok := m.answers[msg.question]; ok {
			last.failed = true
			m.answers[msg.question] = last
		}
		m.err = "answer not recorded: " + msg.err.Error()
		return m, m.refresh()
	}
	delete(m.answers, msg.question)
	m.status = "Answer recorded (" + msg.result.Question.State + ")"
	return m, m.refresh()
}

// openPicker lists the threads to switch to.
func (m Model) openPicker() (Model, tea.Cmd) {
	m.focus = focusPicker
	m.input.Blur()
	m.picker = pickerState{loading: true}
	ctx, api := m.ctx, m.api
	return m, func() tea.Msg {
		threads, err := api.Threads(ctx)
		return threadsMsg{threads: threads, err: err}
	}
}

func (m Model) listed(msg threadsMsg) (Model, tea.Cmd) {
	if m.focus != focusPicker {
		return m, nil
	}
	m.picker.loading = false
	if msg.err != nil {
		m.err = "could not list threads: " + msg.err.Error()
		m.closePicker()
		return m, nil
	}
	var open []clientapi.Thread
	for _, t := range msg.threads {
		if !t.Archived {
			open = append(open, t)
		}
	}
	m.picker.threads = open
	for i, t := range open {
		if t.ID == m.thread.ID {
			m.picker.selected = i + 1
		}
	}
	return m, nil
}

// pickerKey moves through the list; row 0 starts a new thread.
func (m Model) pickerKey(press string) (Model, tea.Cmd) {
	rows := len(m.picker.threads) + 1
	switch press {
	case "esc":
		m.closePicker()
	case "up", "k":
		m.picker.selected = (m.picker.selected + rows - 1) % rows
	case "down", "j":
		m.picker.selected = (m.picker.selected + 1) % rows
	case "enter":
		if m.picker.loading {
			return m, nil
		}
		selected := m.picker.selected
		m.closePicker()
		if selected == 0 {
			return m.newThread()
		}
		return m.switchTo(m.picker.threads[selected-1].ID)
	}
	return m, nil
}

func (m *Model) closePicker() {
	m.picker = pickerState{}
	m.focus = focusInput
	m.input.Focus()
	m.pickQuestion()
}

func (m Model) switchTo(id string) (Model, tea.Cmd) {
	m.thread = clientapi.Thread{ID: id}
	m.turns, m.items = nil, nil
	clear(m.progress)
	clear(m.steps)
	m.follow, m.refreshing, m.dirty = true, false, false
	m.err, m.status = "", ""
	return m, m.refresh()
}

func (m Model) newThread() (Model, tea.Cmd) {
	project := m.thread.Project
	if project == "" {
		project = defaultProject(m.projects)
	}
	ctx, api := m.ctx, m.api
	m.refreshing = true
	return m, func() tea.Msg {
		detail, err := api.CreateThread(ctx, clientapi.CreateThread{ID: clientsdk.NewCommandID("tui-"), Project: project})
		return detailMsg{thread: detail.Thread.ID, detail: detail, err: err, opened: true}
	}
}
