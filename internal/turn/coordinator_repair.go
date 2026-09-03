package turn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
)

// Refresher asks a node to check itself again after a repair. The node
// registry satisfies it; the hub's own advert is asked fresh anyway.
type Refresher interface {
	Refresh(ctx context.Context, node string) (nodewire.Advert, error)
}

// Commands runs a shell line on a machine; "" is the hub.
type Commands interface {
	Exec(ctx context.Context, node, dir, command string) (string, error)
}

// SetRepair wires what the repair verb needs beyond the supervisor.
func (c *Coordinator) SetRepair(nodes Refresher, commands Commands) {
	c.refresher = nodes
	c.commands = commands
}

// SetProber wires model discovery: one endpoint on demand (after a repair,
// the harness that just appeared), and all of them for `/fleet probe`.
func (c *Coordinator) SetProber(one func(ctx context.Context, node, harness string) error, all func(ctx context.Context) []models.Result) {
	c.probeOne = one
	c.probeAll = all
}

// probeCmd is `/fleet probe`: ask every harness on every machine what it
// runs, now, and report it. Discovery on purpose, for when a config
// changed or a harness was just installed.
func (c *Coordinator) probeCmd(ctx context.Context) Result {
	title := c.text.T(i18n.CardFleet)
	if c.probeAll == nil {
		return Result{Title: title, Text: c.text.T(i18n.FleetLocal)}
	}
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	results := c.probeAll(ctx)
	if len(results) == 0 {
		return Result{Title: title, Text: c.text.T(i18n.FleetProbeNothing)}
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		where := nodewire.Place(r.Endpoint.Node) + " · " + r.Endpoint.Harness
		switch {
		case r.Err != nil:
			fmt.Fprintf(&b, "✗ **%s**\n> %s", where, r.Err)
		case r.Current == "" && len(r.Available) == 0:
			fmt.Fprintf(&b, "· **%s**\n%s", where, c.text.T(i18n.FleetProbeSilent))
		default:
			fmt.Fprintf(&b, "✓ **%s** · %s", where, r.Current)
			if len(r.Available) > 0 {
				fmt.Fprintf(&b, "\n%s", strings.Join(r.Available, ", "))
			}
		}
	}
	return Result{Title: title, Text: b.String()}
}

// repairCmd has a healthy agent on the same machine fix a broken harness.
//
// The shape is a one-step plan pinned to the helper, verified by a command
// on that machine: the fix counts when `command -v` finds the binary, not
// when the agent says it is done. Afterwards the machine is asked to check
// itself again, so the fleet shows the repaired agent as ready — or still
// blocked, with the reason.
func (c *Coordinator) repairCmd(ctx context.Context, req Request, rest string) Result {
	title := c.text.T(i18n.CardRepair)
	agentID := strings.TrimSpace(rest)
	if agentID == "" {
		return Result{Title: title, Text: c.text.T(i18n.RepairUsage, protocol.CommandRepair)}
	}
	if c.supervisor == nil || c.plans == nil || c.fleet == nil {
		return Result{Title: title, Text: c.text.T(i18n.RepairDisabled)}
	}
	fix, err := c.fleet.Repair(ctx, agentID)
	if errors.Is(err, roster.ErrNotBroken) {
		return Result{Title: title, Text: c.text.T(i18n.RepairNotNeeded, agentID)}
	}
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.RepairImpossible, agentID, err)}
	}
	machine := nodewire.Place(fix.Broken.Node)
	path := c.pathOn(ctx, fix.Broken.Node)
	goal := c.text.T(i18n.RepairGoal, machine, fix.Broken.Harness, fix.Broken.Missing, fix.Command, path, fix.Command)

	// A repair is about a machine, not about whatever project the chat is
	// in: it runs under a project that machine may hold, or not at all.
	projectID, err := c.repairProject(ctx, fix)
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.RepairImpossible, agentID, err)}
	}
	tracked, err := c.openPlanTask(req, "repair "+agentID+" on "+machine, projectID)
	if err != nil {
		return Result{Title: title, Text: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()

	proposed := plan.Plan{
		TaskID: tracked.ID, ProjectID: tracked.ProjectID, Goal: goal,
		By: "repair", Because: fix.Broken.Missing, Fixed: true,
		Steps: []plan.Step{{
			ID: "repair", Goal: goal, Agent: fix.Helper.Agent.ID,
			Verify: &plan.Verify{Kind: plan.VerifyCommand, Command: "command -v " + shellQuote(fix.Command)},
		}},
	}
	if base, err := c.planBase(ctx, tracked); err != nil {
		return Result{Title: title, Text: c.text.T(i18n.RepairStopped, agentID, err)}
	} else {
		proposed.Base = base
	}
	stored, err := c.plans.Create(proposed)
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.RepairStopped, agentID, err)}
	}
	outcome, runErr := c.supervisor.Execute(ctx, stored)
	final, _ := c.plans.Latest(stored.ID)
	if runErr != nil {
		return Result{Title: title, Text: c.text.T(i18n.RepairStopped, agentID, runErr) + "\n\n" + c.planTree(final, outcome)}
	}
	if _, err := c.tasks.Advance(tracked.ID, task.StateDone); err != nil {
		log.Printf("turn: close repair task %s: %v", tracked.ID, err)
	}
	// The verify command passed on that machine; now let the machine say
	// so itself, which is what every placement decision reads.
	if fix.Broken.Node != "" && c.refresher != nil {
		if _, err := c.refresher.Refresh(ctx, fix.Broken.Node); err != nil {
			log.Printf("turn: refresh %s after repair: %v", fix.Broken.Node, err)
		}
	}
	// A harness that just started existing has never reported a model;
	// ask it, so the fleet's column fills without waiting for real work.
	if c.probeOne != nil {
		if err := c.probeOne(ctx, fix.Broken.Node, fix.Broken.Harness); err != nil {
			log.Printf("turn: probe %s after repair: %v", agentID, err)
		}
	}
	for _, item := range c.fleet.All(ctx) {
		if item.Agent.ID != agentID {
			continue
		}
		if item.Eligible {
			return Result{Title: title, Text: c.text.T(i18n.RepairDone, agentID, fix.Helper.Agent.ID, fix.Command) + "\n\n" + c.planTree(final, outcome)}
		}
		return Result{Title: title, Text: c.text.T(i18n.RepairStillBroken, agentID, item.Why) + "\n\n" + c.planTree(final, outcome)}
	}
	return Result{Title: title, Text: c.text.T(i18n.RepairStillBroken, agentID, "agent vanished from the roster")}
}

// repairProject picks the project a repair runs under: the one homed on
// the broken agent's machine when there is one, else any project whose
// level admits that machine. The chat's own project would usually be the
// wrong one — a restricted project cannot be worked on an internal node,
// which is exactly where a repair has to happen.
func (c *Coordinator) repairProject(ctx context.Context, fix roster.Fix) (string, error) {
	if c.projects == nil {
		return "", fmt.Errorf("%s", c.text.T(i18n.ProjectsDisabled))
	}
	all, err := c.projects.List(ctx)
	if err != nil {
		return "", err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	for _, p := range all {
		if p.Home.Node == fix.Broken.Node {
			return p.ID, nil
		}
	}
	for _, p := range all {
		if p.Level.OrDefault().Admits(fix.Broken.Level.OrDefault()) {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("no project may be worked on %s (level %s)", nodewire.Place(fix.Broken.Node), fix.Broken.Level.OrDefault())
}

// pathOn reads the PATH the steve process on a machine actually has, so
// the helper installs somewhere that process will look.
func (c *Coordinator) pathOn(ctx context.Context, node string) string {
	if c.commands == nil {
		return "(unknown)"
	}
	out, err := c.commands.Exec(ctx, node, "", "printf '%s' \"$PATH\"")
	if err != nil || strings.TrimSpace(out) == "" {
		return "(unknown)"
	}
	return strings.TrimSpace(out)
}

// planBase snapshots the project's canonical workspace as the plan's
// starting point, the way /plan does.
func (c *Coordinator) planBase(ctx context.Context, tracked task.Task) (string, error) {
	if c.artifacts == nil {
		return "", nil
	}
	p, ok, err := c.projects.Get(ctx, tracked.ProjectID)
	if err != nil || !ok {
		return "", err
	}
	base, _, err := c.artifacts.SnapshotCanonical(ctx, p, c.artifacts.CanonicalOf(ctx, p.ID), "plan", "base of plan for task #"+tracked.ID)
	if err != nil {
		return "", err
	}
	return base.ID, nil
}

// shellQuote makes one word safe for /bin/sh.
func shellQuote(word string) string {
	if word == "" {
		return "''"
	}
	if strings.IndexFunc(word, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./+:@%", r))
	}) < 0 {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// repairHint is the /fleet line under a blocked agent that says who could
// fix it, so the answer to "why is kimi blocked" comes with a next step.
func (c *Coordinator) repairHint(ctx context.Context, item roster.Candidate) string {
	if item.Eligible || c.fleet == nil {
		return ""
	}
	fix, err := c.fleet.Repair(ctx, item.Agent.ID)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\n%s", c.text.T(i18n.FleetRepairHint, protocol.CommandRepair, item.Agent.ID, fix.Helper.Agent.ID))
}
