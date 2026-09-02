package exec

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/view"
)

// Commands runs a shell command on a machine. The node registry satisfies
// it; an empty node is the hub.
type Commands interface {
	Exec(ctx context.Context, node, dir, command string) (string, error)
}

// Verifiers is the executor's verification: a command run where the work is,
// or a second agent asked to check the first one's. Neither takes the
// working agent's word for anything — that is the whole point.
type Verifiers struct {
	commands   Commands
	sessions   Sessions
	roster     *roster.Roster
	workspaces project.Workspaces
	// Timeout bounds one verification. Zero takes the default.
	Timeout time.Duration
}

func NewVerifiers(commands Commands, sessions Sessions, r *roster.Roster, workspaces project.Workspaces) *Verifiers {
	return &Verifiers{commands: commands, sessions: sessions, roster: r, workspaces: workspaces}
}

const defaultVerifyTimeout = 10 * time.Minute

func (v *Verifiers) Verify(ctx context.Context, req StepRequest, check plan.Verify, result plan.StepResult) error {
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = defaultVerifyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch check.Kind {
	case plan.VerifyCommand:
		return v.byCommand(ctx, req, check)
	case plan.VerifyAgent:
		return v.byAgent(ctx, req, check, result)
	case plan.VerifyNone:
		return nil
	default:
		return fmt.Errorf("unknown verify kind %q", check.Kind)
	}
}

// byCommand runs the check on the machine the step ran on, in the agent's
// workspace. Running it on the hub would check a different filesystem.
func (v *Verifiers) byCommand(ctx context.Context, req StepRequest, check plan.Verify) error {
	if v.commands == nil {
		return fmt.Errorf("no way to run commands on %s", nodeLabel(req.Node))
	}
	out, err := v.commands.Exec(ctx, req.Node, req.Workspace, check.Command)
	if err != nil {
		return fmt.Errorf("%q on %s: %w", check.Command, nodeLabel(req.Node), err)
	}
	_ = out
	return nil
}

// byAgent asks a different agent whether the work holds up. The answer is
// held to one word so it cannot be hedged into a pass.
func (v *Verifiers) byAgent(ctx context.Context, req StepRequest, check plan.Verify, result plan.StepResult) error {
	if v.sessions == nil {
		return fmt.Errorf("no way to open a session for verifier %s", check.Agent)
	}
	if check.Agent == req.Agent {
		return fmt.Errorf("a step cannot be verified by the agent that did it")
	}
	c, ok := v.candidate(ctx, check.Agent)
	if !ok {
		return fmt.Errorf("verifier %q is not in the roster", check.Agent)
	}
	if !c.Eligible {
		return fmt.Errorf("verifier %s cannot run now: %s", check.Agent, c.Why)
	}
	at := harness.Placement{Node: c.Node, Harness: c.Harness}
	if v.workspaces == nil {
		return fmt.Errorf("no workspaces wired: verifier %s has nowhere to run", check.Agent)
	}
	// The verifier reads exactly what was published, in its own worktree
	// on its own machine; nothing it does can reach the step's tree.
	workspace, err := v.workspaces.Materialize(ctx, project.Request{Project: req.Project, Node: c.Node, Isolated: true, Base: result.Artifact, Owner: "verify-" + req.StepID})
	if err != nil {
		return fmt.Errorf("workspace for verifier %s: %w", check.Agent, err)
	}
	defer discardWorkspace(ctx, v.workspaces, workspace)
	session, err := v.sessions.OpenSession(ctx, at, "", workspace.Path, nil)
	if err != nil {
		return fmt.Errorf("open verifier session on %s: %w", at, err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer closeCancel()
		if err := v.sessions.CloseSession(closeCtx, at, session.ID()); err != nil {
			session.Abort()
		}
	}()
	prompt := verifyBrief(req, result)
	answer, _, err := session.Prompt(ctx, prompt, func(view.Progress) {})
	if err != nil {
		return fmt.Errorf("verifier %s: %w", check.Agent, err)
	}
	verdict, reason := parseVerdict(answer)
	switch verdict {
	case "PASS":
		return nil
	case "FAIL":
		return fmt.Errorf("verifier %s: %s", check.Agent, reason)
	default:
		return fmt.Errorf("verifier %s gave no verdict: %s", check.Agent, strings.TrimSpace(firstLine(answer)))
	}
}

func (v *Verifiers) candidate(ctx context.Context, id string) (roster.Candidate, bool) {
	if v.roster == nil {
		return roster.Candidate{}, false
	}
	for _, c := range v.roster.All(ctx) {
		if c.Agent.ID == id {
			return c, true
		}
	}
	return roster.Candidate{}, false
}

func verifyBrief(req StepRequest, result plan.StepResult) string {
	var b strings.Builder
	b.WriteString("你是审核者。另一个 agent 声称完成了下面这一步，请独立核实，不要相信它的自述。\n\n")
	fmt.Fprintf(&b, "## 这一步\n%s\n\n", req.Goal)
	fmt.Fprintf(&b, "## 它的交代\n%s\n\n", strings.TrimSpace(result.Answer))
	if len(result.Refs) > 0 {
		b.WriteString("## 它留下的引用\n")
		for _, ref := range result.Refs {
			fmt.Fprintf(&b, "- %s: %s\n", ref.Kind, ref.Value)
		}
		b.WriteString("\n")
	}
	b.WriteString("## 回答格式\n第一行只写 PASS 或 FAIL。FAIL 时第二行起写明哪里不成立。\n")
	return b.String()
}

func parseVerdict(answer string) (string, string) {
	lines := strings.Split(strings.TrimSpace(answer), "\n")
	for i, line := range lines {
		word := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(line, "echo:")))
		switch {
		case strings.HasPrefix(word, "PASS"):
			return "PASS", ""
		case strings.HasPrefix(word, "FAIL"):
			return "FAIL", strings.TrimSpace(strings.Join(lines[i+1:], " "))
		}
	}
	return "", ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func nodeLabel(node string) string {
	if node == "" {
		return "hub"
	}
	return node
}

func discardWorkspace(ctx context.Context, w project.Workspaces, ws project.Workspace) {
	if d, ok := w.(interface {
		Discard(context.Context, project.Workspace) error
	}); ok && ws.Kind == project.KindWorktree {
		_ = d.Discard(context.WithoutCancel(ctx), ws)
	}
}
