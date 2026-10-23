package tuiapp

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/gopact-ai/steve/internal/clientapi"
	"github.com/gopact-ai/steve/internal/clientsdk"
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	faint       = lipgloss.NewStyle().Faint(true)
	userStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	steveStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	selected    = lipgloss.NewStyle().Reverse(true)
)

// View draws the header, the transcript, then the question or thread
// picker when one is open, the status line and the input box.
func (m Model) View() tea.View {
	parts := []string{m.header(), m.transcript.View()}
	if overlay := m.overlay(); overlay != "" {
		parts = append(parts, overlay)
	}
	parts = append(parts, m.statusLine(), m.input.View())
	v := tea.NewView(strings.Join(parts, "\n"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "Steve"
	return v
}

// layout sizes the transcript to what the other parts leave and renders
// it, keeping the view at the bottom while the user has not scrolled up.
func (m *Model) layout() {
	width := max(m.width, 20)
	m.input.SetWidth(width)
	m.question.text.SetWidth(max(width-14, 10))
	m.clampQuestion()
	used := lipgloss.Height(m.header()) + lipgloss.Height(m.statusLine()) + m.input.Height()
	if overlay := m.overlay(); overlay != "" {
		used += lipgloss.Height(overlay)
	}
	m.transcript.SetWidth(width)
	m.transcript.SetHeight(max(m.height-used, 3))
	m.transcript.SetContent(m.render(width))
	if m.follow {
		m.transcript.GotoBottom()
	}
}

func (m Model) header() string {
	project, agent, node := m.thread.Project, m.thread.Agent, ""
	if agent == "" {
		for _, a := range m.agents {
			if a.Default {
				agent = a.ID
			}
		}
	}
	for _, a := range m.agents {
		if a.ID == agent {
			node = a.Node
		}
	}
	if running := m.runningTurn(); running != nil {
		if p := m.progress[running.ID]; p != nil {
			agent, node = orKeep(p.Agent, agent), orKeep(p.Node, node)
		}
	}
	title := m.thread.Title
	if title == "" {
		title = m.thread.ID
	}
	link := m.link.String()
	switch m.link {
	case clientsdk.Connected:
		link = okStyle.Render("● " + link)
	case clientsdk.Disconnected:
		link = errStyle.Render("● " + link)
	default:
		link = warnStyle.Render("● " + link)
	}
	// The connection state comes first so a narrow terminal never cuts it.
	fields := []string{headerStyle.Render("Steve"), link, title}
	for _, f := range [][2]string{{"project", project}, {"agent", agent}, {"node", node}} {
		if f[1] != "" {
			fields = append(fields, faint.Render(f[0]+" ")+f[1])
		}
	}
	if m.thread.ReadOnly {
		fields = append(fields, warnStyle.Render("read-only"))
	}
	return lipgloss.NewStyle().MaxWidth(max(m.width, 20)).Render(strings.Join(fields, faint.Render(" · ")))
}

func orKeep(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func (m Model) statusLine() string {
	var parts []string
	if running := m.runningTurn(); running != nil {
		phase := "working"
		if running.Phase != "" {
			phase = running.Phase
		} else if p := m.progress[running.ID]; p != nil && p.Phase != "" {
			phase = p.Phase
		}
		parts = append(parts, warnStyle.Render("◐ "+phase))
	}
	if n := m.queuedTurns() + len(m.sending()); n > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", n))
	}
	if problem := m.problem(); problem != "" {
		parts = append(parts, errStyle.Render(problem))
	} else if m.status != "" {
		parts = append(parts, m.status)
	}
	if len(parts) == 0 {
		parts = append(parts, faint.Render("Enter send · Shift/Alt+Enter newline · Ctrl-C interrupt · Ctrl-T threads · Ctrl-D quit"))
	}
	return lipgloss.NewStyle().MaxWidth(max(m.width, 20)).Render(strings.Join(parts, "  "))
}

// render draws the transcript: the server's items, then what the running
// turn reports, then input sent but not yet acknowledged.
func (m Model) render(width int) string {
	if !m.loaded {
		if m.problem() != "" {
			return ""
		}
		return faint.Render("Connecting to Steve…")
	}
	wrap := lipgloss.NewStyle().Width(width)
	var b strings.Builder
	for _, item := range m.items {
		if block := renderItem(item); block != "" {
			b.WriteString(wrap.Render(block))
			b.WriteString("\n\n")
		}
	}
	if running := m.runningTurn(); running != nil {
		if live := m.renderProgress(running.ID); live != "" {
			b.WriteString(wrap.Render(live))
			b.WriteString("\n\n")
		}
	}
	for _, s := range m.sending() {
		b.WriteString(wrap.Render(userStyle.Render("you") + faint.Render(" (sending)") + "\n" + s.req.Input))
		b.WriteString("\n\n")
	}
	if b.Len() == 0 {
		return faint.Render("No messages yet. Type below, or /help to see what Steve can do.")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderItem(item clientapi.Item) string {
	switch item.Kind {
	case clientapi.ItemMessage:
		if item.Role == "user" {
			return userStyle.Render("you") + "\n" + item.Text
		}
		head := steveStyle.Render("steve")
		if item.Title != "" {
			head += " " + headerStyle.Render(item.Title)
		}
		body := item.Text
		if item.Error != "" && trimmed(item.Error) != trimmed(item.Text) {
			body = strings.TrimSpace(body + "\n" + errStyle.Render(item.Error))
		}
		return head + "\n" + body
	case clientapi.ItemReasoning:
		return faint.Render("thinking: " + oneLine(item.Text, 300))
	case clientapi.ItemToolCall:
		if item.Tool == nil {
			return ""
		}
		return renderTool(*item.Tool)
	case clientapi.ItemFileChange:
		if item.Change == nil || item.Change.Files == 0 {
			return ""
		}
		line := fmt.Sprintf("changed %d file(s)", item.Change.Files)
		if item.Change.Added != nil && item.Change.Deleted != nil {
			line += fmt.Sprintf(" +%d -%d", *item.Change.Added, *item.Change.Deleted)
		}
		return faint.Render("◇ " + line)
	case clientapi.ItemQuestion:
		q := item.Question
		if q == nil {
			return ""
		}
		line := "? " + questionTitle(q)
		if msg := oneLine(q.Message, 200); msg != "" {
			line += ": " + msg
		}
		if q.State == questionPending {
			return warnStyle.Render(line + " (waiting for you)")
		}
		return faint.Render(line + " — " + q.State + answerLabel(q))
	case clientapi.ItemMilestone, clientapi.ItemNotice:
		text := item.Text
		if item.Title != "" {
			text = item.Title + ": " + text
		}
		return faint.Render("— " + text)
	}
	return ""
}

func answerLabel(q *clientapi.Question) string {
	if q.Answer == nil {
		return ""
	}
	if q.Answer.Text != "" {
		return " (" + oneLine(q.Answer.Text, 60) + ")"
	}
	for _, o := range q.Options {
		if o.ID == q.Answer.Choice {
			return " (" + o.Label + ")"
		}
	}
	return ""
}

func renderTool(t clientapi.ToolCall) string {
	mark := "·"
	switch t.Status {
	case "completed", "done":
		mark = "✓"
	case "failed":
		mark = "✗"
	case "running", "queued":
		mark = "…"
	}
	name := orKeep(t.Name, t.Kind)
	if t.Detail != "" {
		name += " " + oneLine(t.Detail, 120)
	}
	where := ""
	if t.Agent != "" {
		where = " @" + t.Agent
		if t.Node != "" {
			where += "/" + t.Node
		}
	}
	return faint.Render(mark + " " + name + where)
}

// renderProgress is what a running turn and its delegated steps reported
// last: the plan, the tools, and the answer so far.
func (m Model) renderProgress(turn string) string {
	var lines []string
	if p := m.progress[turn]; p != nil {
		for _, line := range p.Plan {
			mark := "☐"
			if line.Status == "completed" {
				mark = "☑"
			}
			lines = append(lines, faint.Render(mark+" "+line.Text))
		}
		if p.Reasoning != "" && p.Answer == "" {
			lines = append(lines, faint.Render("thinking: "+oneLine(lastChars(p.Reasoning, 300), 300)))
		}
		for _, t := range p.Tools {
			lines = append(lines, renderTool(t))
		}
	}
	steps := m.steps[turn]
	ids := make([]string, 0, len(steps))
	for id := range steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := steps[id]
		label := "step " + id
		if s.Agent != "" {
			label += " @" + s.Agent
		}
		if s.Phase != "" {
			label += " " + s.Phase
		}
		lines = append(lines, faint.Render("↳ "+label))
	}
	if p := m.progress[turn]; p != nil && p.Answer != "" {
		lines = append(lines, steveStyle.Render("steve")+faint.Render(" (writing)")+"\n"+p.Answer)
	}
	return strings.Join(lines, "\n")
}

// overlay is the question prompt or the thread picker.
func (m Model) overlay() string {
	if m.focus == focusPicker {
		return m.pickerView()
	}
	q := m.currentQuestion()
	if q == nil {
		return ""
	}
	lines, _ := m.questionBody()
	rows := m.questionRows()
	start := min(m.question.scroll, max(len(lines)-rows, 0))
	end := min(len(lines), start+rows)
	shown := append([]string(nil), lines[start:end]...)
	if last, ok := m.answers[q.ID]; ok && !last.failed {
		shown = append(shown, faint.Render("sending answer…"))
	}
	focused := m.focus == focusQuestion
	hint := "↑/↓ choose · Enter accept · d decline · c cancel · Esc later"
	if focused && m.question.writing {
		hint = "Enter send · Esc leave the answer"
	}
	if len(lines) > rows {
		hint = fmt.Sprintf("lines %d-%d of %d · PgUp/PgDn scroll · ", start+1, end, len(lines)) + hint
	}
	if !focused {
		hint = "Tab to answer"
	}
	shown = append(shown, faint.Render(hint))
	return boxStyle.Width(max(m.width, 20)).Render(strings.Join(shown, "\n"))
}

// questionRows is how many lines of the question the overlay shows; the
// rest scrolls. It leaves room for the transcript, status and input.
func (m Model) questionRows() int {
	return max(m.height/2-4, 3)
}

// questionBody is the question as wrapped lines and the line of the
// selected choice.
func (m Model) questionBody() ([]string, int) {
	q := m.currentQuestion()
	if q == nil {
		return nil, 0
	}
	wrap := lipgloss.NewStyle().Width(max(m.width-4, 10))
	head := headerStyle.Render(questionTitle(q))
	if q.Agent != "" {
		head += faint.Render("  from " + q.Agent)
	}
	lines := strings.Split(wrap.Render(head), "\n")
	if msg := trimmed(q.Message); msg != "" {
		lines = append(lines, strings.Split(wrap.Render(msg), "\n")...)
	}
	lines = append(lines, "")
	focused := m.focus == focusQuestion
	at := len(lines)
	for i, o := range q.Options {
		row := "  " + o.Label
		if o.Description != "" {
			row += faint.Render(" — " + o.Description)
		}
		if focused && !m.question.writing && i == m.question.selected {
			row = selected.Render("› " + o.Label)
		}
		if i == m.question.selected {
			at = len(lines)
		}
		lines = append(lines, strings.Split(wrap.Render(row), "\n")...)
	}
	if q.AllowFreeText {
		if m.question.writing || m.question.selected == len(q.Options) {
			at = len(lines)
		}
		if m.question.writing {
			lines = append(lines, m.question.text.View())
		} else {
			row := "  Write an answer…"
			if focused && m.question.selected == len(q.Options) {
				row = selected.Render("› Write an answer…")
			}
			lines = append(lines, row)
		}
	}
	return lines, at
}

// clampQuestion keeps the question's scroll in range and, after the
// selection moved, the selected choice in view.
func (m *Model) clampQuestion() {
	lines, at := m.questionBody()
	rows := m.questionRows()
	if m.question.pin {
		if at < m.question.scroll {
			m.question.scroll = at
		} else if at >= m.question.scroll+rows {
			m.question.scroll = at - rows + 1
		}
		m.question.pin = false
	}
	m.question.scroll = max(0, min(m.question.scroll, len(lines)-rows))
}

func (m Model) pickerView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Threads"))
	if m.picker.loading {
		b.WriteString("\n" + faint.Render("loading…"))
		return boxStyle.Width(max(m.width, 20)).Render(b.String())
	}
	rows := []string{"+ New thread"}
	for _, t := range m.picker.threads {
		label := orKeep(t.Title, t.ID)
		meta := []string{}
		if t.Running {
			meta = append(meta, "running")
		}
		if t.PendingQuestions > 0 {
			meta = append(meta, fmt.Sprintf("%d waiting", t.PendingQuestions))
		}
		if t.ReadOnly {
			meta = append(meta, t.Transport+", read-only")
		}
		if t.Project != "" {
			meta = append(meta, t.Project)
		}
		if len(meta) > 0 {
			label += faint.Render("  " + strings.Join(meta, " · "))
		}
		rows = append(rows, label)
	}
	// Show a window of rows around the selection.
	limit := max(m.height/2, 5)
	start := max(0, min(m.picker.selected-limit/2, len(rows)-limit))
	end := min(len(rows), start+limit)
	for i := start; i < end; i++ {
		row := "  " + rows[i]
		if i == m.picker.selected {
			row = selected.Render("› " + threadLabel(m.picker, i))
		}
		b.WriteString("\n" + row)
	}
	b.WriteString("\n" + faint.Render("↑/↓ choose · Enter open · Esc close"))
	return boxStyle.Width(max(m.width, 20)).Render(b.String())
}

func threadLabel(p pickerState, row int) string {
	if row == 0 {
		return "+ New thread"
	}
	t := p.threads[row-1]
	return orKeep(t.Title, t.ID)
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}

func lastChars(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return s
}

func questionTitle(q *clientapi.Question) string {
	if q.Title != "" {
		return q.Title
	}
	if q.Kind == "permission" {
		return "Permission requested"
	}
	return "Question"
}
