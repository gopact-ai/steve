package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
)

// Commands runs a shell command on a machine. The node registry satisfies
// it; an empty node is the hub.
type Commands interface {
	Exec(ctx context.Context, node, dir, command string) (string, error)
}

// Verifiers is the executor's verification: a command run where the work is,
// or a second agent asked to check the first one's. Neither takes the
// working agent's word for anything — that is the whole point.
type AgentVerifier interface {
	Prompt(context.Context, agentexec.Spec, string, func(string) error) (agentexec.Result, error)
}

type Verifiers struct {
	commands Commands
	executor AgentVerifier
	Timeout  time.Duration
}

func NewVerifiers(commands Commands, executor AgentVerifier) *Verifiers {
	return &Verifiers{commands: commands, executor: executor}
}

const DefaultVerifyTimeout = 10 * time.Minute

func (v *Verifiers) Verify(ctx context.Context, req StepRequest, check plan.Verify, result plan.StepResult) error {
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = DefaultVerifyTimeout
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
		var exit interface{ ExitCode() int }
		if req.Node != "" && !errors.As(err, &exit) {
			err = errors.Join(harness.ErrStopUnconfirmed, err)
		}
		return fmt.Errorf("%q on %s: %w", check.Command, nodeLabel(req.Node), err)
	}
	_ = out
	return nil
}

// byAgent asks a different agent whether the work holds up. The answer is
// held to one word so it cannot be hedged into a pass.
func (v *Verifiers) byAgent(ctx context.Context, req StepRequest, check plan.Verify, result plan.StepResult) error {
	if v.executor == nil {
		return fmt.Errorf("agent verification execution is not configured")
	}
	if check.Agent == req.Agent {
		return fmt.Errorf("a step cannot be verified by the agent that did it")
	}
	_, err := v.executor.Prompt(ctx, agentexec.Spec{TaskID: req.TaskID, TurnID: req.PlanID + "/" + req.StepID + "/verify", Agent: check.Agent, Project: req.Project, Base: result.Artifact, Kind: attempt.KindVerify, Timeout: v.Timeout}, verifyBrief(req, result), func(answer string) error {
		verdict, reason := parseVerdict(answer)
		switch verdict {
		case "PASS":
			return nil
		case "FAIL":
			return fmt.Errorf("verifier %s rejected: %s", check.Agent, reason)
		default:
			return fmt.Errorf("verifier %s did not return PASS or FAIL: %s", check.Agent, firstLine(answer))
		}
	})
	return err
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

func nodeLabel(node string) string { return nodewire.Place(node) }
