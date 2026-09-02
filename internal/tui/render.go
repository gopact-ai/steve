package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
)

// ANSI is used directly rather than through a framework: the whole screen is
// a handful of tables, and a dependency for that would be the larger cost.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	green  = "\x1b[32m"
	red    = "\x1b[31m"
	yellow = "\x1b[33m"
	cyan   = "\x1b[36m"
	violet = "\x1b[35m"
)

func (m *Model) render() string {
	m.mu.Lock()
	snap, feed, live, lastErr := m.snap, append([]readmodel.Event{}, m.feed...), m.live, m.lastErr
	width, height, mode, oneShot := m.width, m.height, m.viewMode, m.oneShot
	m.mu.Unlock()

	var b strings.Builder
	b.WriteString(m.header(snap, live, oneShot, lastErr, width))
	switch mode {
	case viewPlans:
		b.WriteString(renderPlans(snap, width))
		b.WriteString(renderLandings(snap, width))
	case viewFeed:
		b.WriteString(renderFeed(feed, height-6))
	case viewLedger:
		b.WriteString(renderLedger(snap, width))
	default:
		b.WriteString(renderNodes(snap, width))
		b.WriteString(renderAgents(snap, width))
		b.WriteString(renderAttempts(snap, width))
		b.WriteString(renderTasks(snap, width))
	}
	b.WriteString(footer(mode, width))
	return b.String()
}

func (m *Model) header(snap readmodel.Snapshot, live, oneShot bool, lastErr string, width int) string {
	state := green + "● live" + reset
	switch {
	case oneShot:
		state = dim + "○ snapshot" + reset
	case !live:
		state = red + "● offline" + reset
	}
	hub := snap.Hub.Node
	if hub == "" {
		hub = "—"
	}
	up, total := 0, len(snap.Nodes)
	for _, n := range snap.Nodes {
		if n.Up {
			up++
		}
	}
	line := fmt.Sprintf("%ssteve%s  hub %s  ·  nodes %d/%d up  ·  %s   %s",
		bold, reset, hub, up, total, time.Now().Format("15:04:05"), state)
	out := line + "\n"
	if lastErr != "" {
		out += red + "  " + truncate(lastErr, width-4) + reset + "\n"
	}
	return out + dim + strings.Repeat("─", max(10, min(width, 120))) + reset + "\n"
}

func renderNodes(snap readmodel.Snapshot, width int) string {
	var b strings.Builder
	b.WriteString("\n" + bold + "NODES" + reset + "\n")
	if len(snap.Nodes) == 0 {
		return b.String() + dim + "  none configured — every agent runs on the hub\n" + reset
	}
	for _, n := range sortedNodes(snap.Nodes) {
		mark := green + "up  " + reset
		if !n.Up {
			mark = red + "down" + reset
		}
		fmt.Fprintf(&b, "  %s  %-10s %-20s %-12s %s\n",
			mark, n.Name, dim+n.Addr+reset, n.OS+"/"+n.Arch,
			cyan+strings.Join(n.Capabilities, ",")+reset)
		for _, h := range n.Harnesses {
			if h.Missing != "" {
				fmt.Fprintf(&b, "        %s%s ✗ %s%s\n", red, h.ID, truncate(h.Missing, width-20), reset)
				continue
			}
			fmt.Fprintf(&b, "        %s%s%s %s\n", dim, h.ID, reset,
				dim+strings.Join(h.Models, ",")+reset)
		}
		if n.LastError != "" {
			fmt.Fprintf(&b, "        %s%s%s\n", red, truncate(n.LastError, width-10), reset)
		}
	}
	return b.String()
}

func renderAgents(snap readmodel.Snapshot, width int) string {
	var b strings.Builder
	b.WriteString("\n" + bold + "AGENTS" + reset + "\n")
	agents := append([]readmodel.Agent{}, snap.Agents...)
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	for _, a := range agents {
		mark := green + "ready  " + reset
		if !a.Eligible {
			mark = yellow + "blocked" + reset
		}
		where := a.Node
		if where == "" {
			where = "hub"
		}
		fmt.Fprintf(&b, "  %s  %-12s %-10s %-12s %-10s %s\n",
			mark, a.ID, where, a.Harness, levelSlots(a), dim+a.Model+reset)
		if a.Why != "" {
			fmt.Fprintf(&b, "        %s%s%s\n", yellow, truncate(a.Why, width-10), reset)
		}
	}
	return b.String()
}

// renderTasks draws the tree, because delegated work is the case a flat list
// hides — and the tree is the whole debugging story for it.
func renderTasks(snap readmodel.Snapshot, width int) string {
	var b strings.Builder
	b.WriteString("\n" + bold + "TASKS" + reset + "\n")
	if len(snap.Tasks) == 0 {
		return b.String() + dim + "  nothing running\n" + reset
	}
	byID := map[string]readmodel.Task{}
	for _, t := range snap.Tasks {
		byID[t.ID] = t
	}
	var walk func(t readmodel.Task, depth int)
	walk = func(t readmodel.Task, depth int) {
		indent := strings.Repeat("  ", depth)
		branch := ""
		if depth > 0 {
			branch = "└ "
		}
		fmt.Fprintf(&b, "  %s%s%s#%s%s %s  %-10s %-9s %s\n",
			indent, branch, bold, t.ID, reset, stateMark(t.State),
			t.Member, whereOf(t.NodeID), budgetBar(t.Turns, t.MaxTurns)+dim+
				fmt.Sprintf(" %d/%d · %s", t.Turns, t.MaxTurns, t.Elapsed)+reset)
		if t.Goal != "" {
			fmt.Fprintf(&b, "  %s    %s%s%s\n", indent, dim, truncate(t.Goal, width-12), reset)
		}
		for _, id := range t.Children {
			if child, ok := byID[id]; ok {
				walk(child, depth+1)
			}
		}
	}
	for _, t := range snap.Tasks {
		if t.Parent == "" || byID[t.Parent].ID == "" {
			walk(t, 0)
		}
	}
	return b.String()
}

func renderPlans(snap readmodel.Snapshot, width int) string {
	var b strings.Builder
	b.WriteString("\n" + bold + "PLANS" + reset + "\n")
	if len(snap.Plans) == 0 {
		return b.String() + dim + "  no plans\n" + reset
	}
	for _, p := range snap.Plans {
		fmt.Fprintf(&b, "\n  %splan %s%s rev %d  %s· task #%s · by %s%s\n",
			bold, p.ID, reset, p.Rev, dim, p.TaskID, p.By, reset)
		if p.Because != "" {
			fmt.Fprintf(&b, "    %s%s%s\n", dim, truncate("because: "+p.Because, width-6), reset)
		}
		for _, s := range p.Steps {
			deps := append(append([]string{}, s.Needs...), s.Merge...)
			after := ""
			if len(deps) > 0 {
				after = dim + " after " + strings.Join(deps, ",") + reset
			}
			fmt.Fprintf(&b, "    %s %-12s %-10s %-9s %s%s\n",
				stepMark(s.State), s.ID, s.Agent, whereOf(s.Node),
				truncate(s.Goal, max(10, width-52)), after)
			if s.Error != "" {
				fmt.Fprintf(&b, "        %s%s%s\n", red, truncate(s.Error, width-10), reset)
			}
			if s.Context != nil && len(s.Context.Refs) > 0 {
				fmt.Fprintf(&b, "        %srefs %s%s\n", dim, strings.Join(s.Context.Refs, ", "), reset)
			}
		}
	}
	return b.String()
}

func renderFeed(feed []readmodel.Event, lines int) string {
	var b strings.Builder
	b.WriteString("\n" + bold + "ACTIVITY" + reset + "\n")
	if len(feed) == 0 {
		return b.String() + dim + "  nothing yet\n" + reset
	}
	if lines < 5 {
		lines = 5
	}
	start := max(0, len(feed)-lines)
	for i := len(feed) - 1; i >= start; i-- {
		ev := feed[i]
		fmt.Fprintf(&b, "  %s%s%s  %s %s%s%s %s%s%s\n",
			dim, ev.At.Format("15:04:05"), reset,
			eventMark(ev.Kind), bold, ev.StepID, reset, dim, ev.Detail, reset)
	}
	return b.String()
}

func footer(mode, width int) string {
	names := []string{"overview", "plans", "activity"}
	var tabs []string
	for i, name := range names {
		if i == mode {
			tabs = append(tabs, bold+name+reset)
			continue
		}
		tabs = append(tabs, dim+name+reset)
	}
	return "\n" + dim + strings.Repeat("─", max(10, min(width, 120))) + reset +
		"\n  " + strings.Join(tabs, dim+" · "+reset) +
		dim + "     tab/v switch · r refresh · q quit" + reset + "\n"
}

func stateMark(state string) string {
	switch state {
	case "running":
		return violet + "running  " + reset
	case "done":
		return green + "done     " + reset
	case "failed":
		return red + "failed   " + reset
	case "paused":
		return yellow + "paused   " + reset
	case "cancelled":
		return dim + "cancelled" + reset
	default:
		return dim + fmt.Sprintf("%-9s", state) + reset
	}
}

func stepMark(state string) string {
	switch state {
	case "done":
		return green + "✓" + reset
	case "failed":
		return red + "✗" + reset
	case "running":
		return violet + "●" + reset
	default:
		return dim + "·" + reset
	}
}

func eventMark(kind string) string {
	switch {
	case strings.HasSuffix(kind, "failed"):
		return red + kind + reset
	case strings.HasSuffix(kind, "completed"):
		return green + kind + reset
	case strings.HasSuffix(kind, "started"):
		return violet + kind + reset
	default:
		return dim + kind + reset
	}
}

func budgetBar(used, total int) string {
	const cells = 10
	if total <= 0 {
		return dim + strings.Repeat("·", cells) + reset
	}
	filled := min(cells, used*cells/total)
	color := green
	switch {
	case used*4 >= total*3:
		color = red
	case used*2 >= total:
		color = yellow
	}
	return color + strings.Repeat("█", filled) + reset + dim + strings.Repeat("·", cells-filled) + reset
}

func whereOf(node string) string {
	if node == "" {
		return "hub"
	}
	return node
}

func truncate(s string, width int) string {
	if width < 4 {
		width = 4
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}

func renderAttempts(snap readmodel.Snapshot, width int) string {
	var b strings.Builder
	b.WriteString("\n" + bold + "ATTEMPTS" + reset + "\n")
	if len(snap.Attempts) == 0 {
		return b.String() + dim + "  nothing running\n" + reset
	}
	for _, a := range snap.Attempts {
		fmt.Fprintf(&b, "  %-22s %-9s %-11s %-10s %-9s %s%s%s\n",
			a.ID, a.Kind, a.State, a.Agent, whereOf(a.Node),
			dim, truncate(a.Project+" · "+a.Scope+" · "+strings.Join(a.Leases, ","), max(10, width-66)), reset)
	}
	return b.String()
}

func renderLandings(snap readmodel.Snapshot, width int) string {
	var b strings.Builder
	b.WriteString("\n" + bold + "LANDINGS" + reset + "\n")
	if len(snap.Landings) == 0 {
		return b.String() + dim + "  nothing landed yet\n" + reset
	}
	for _, l := range snap.Landings {
		color := green
		if l.State != "committed" {
			color = red
		}
		fmt.Fprintf(&b, "  %s%-18s%s %-10s %-12s %3d path(s) %s%s%s\n",
			color, l.State, reset, l.Project, l.Artifact[:min(12, len(l.Artifact))], l.Paths, dim, truncate(l.Error, max(10, width-60)), reset)
	}
	return b.String()
}

func levelSlots(a readmodel.Agent) string {
	if a.Slots > 0 {
		return fmt.Sprintf("%s/%d", a.Level, a.Slots)
	}
	return a.Level
}

func renderLedger(snap readmodel.Snapshot, width int) string {
	var b strings.Builder
	f := snap.Facts
	section := func(title string, n int, none string) bool {
		b.WriteString("\n" + bold + title + reset + "\n")
		if n == 0 {
			b.WriteString(dim + "  " + none + "\n" + reset)
			return false
		}
		return true
	}
	if section("DISCLOSURES AWAITING THE OWNER", len(f.Disclosures), "nothing waiting") {
		for _, d := range f.Disclosures {
			fmt.Fprintf(&b, "  %-16s %-12s task #%-6s %s  %d bytes  %s%s%s\n", d.ID, d.Project, d.TaskID, d.Requester, d.Bytes, dim, d.At.Format("15:04:05"), reset)
		}
	}
	if section("EFFECTS WITH AN UNKNOWN OUTCOME", len(f.Effects), "none") {
		for _, e := range f.Effects {
			fmt.Fprintf(&b, "  %-24s %-14s task #%-6s %s%s%s\n", e.ID, e.Tool, e.TaskID, red, truncate(e.Error, max(10, width-56)), reset)
		}
	}
	if section("RESERVATIONS", len(f.Reservations), "no capacity reserved") {
		for _, r := range f.Reservations {
			fmt.Fprintf(&b, "  %-28s %-32s %s%s until %s%s\n", r.ID, r.Endpoint, dim, r.For, r.ExpiresAt.Format("15:04:05"), reset)
		}
	}
	if section("ATTESTATIONS", len(f.Attestations), "no verdicts yet") {
		for _, a := range f.Attestations {
			color := green
			if a.Verdict != "pass" {
				color = red
			}
			fmt.Fprintf(&b, "  %s%-5s%s %-12s %-8s %-10s %s%s%s\n", color, a.Verdict, reset, a.Artifact[:min(12, len(a.Artifact))], a.Step, a.Kind, dim, truncate(a.Verifier, max(10, width-46)), reset)
		}
	}
	if section("REPLICAS", len(f.Replicas), "no copies on nodes") {
		for _, r := range f.Replicas {
			color := green
			if r.State != "verified" {
				color = yellow
			}
			fmt.Fprintf(&b, "  %-12s %-10s gen %-3d %s%-12s%s %s%s%s\n", r.Artifact[:min(12, len(r.Artifact))], r.Node, r.Generation, color, r.State, reset, dim, r.Note, reset)
		}
	}
	if section("GRANTS", len(f.Grants), "no grants; defaults apply") {
		for _, g := range f.Grants {
			fmt.Fprintf(&b, "  %-12s %-28s %-6s %sby %s%s\n", g.Project, g.Principal, g.Role, dim, g.By, reset)
		}
	}
	return b.String()
}
