package work

import (
	"context"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turnapi"
)

// Flow separates conversational selection, platform controls and native
// execution. Work decides which path runs; no adapter calls a harness directly.
type Flow struct {
	Select   func(turnapi.Request) (agent.Agent, string, bool, error)
	Parse    func(string) (string, protocol.Command, string, bool)
	Switched func(agent.Agent) turnapi.Result
	Control  func(context.Context, turnapi.Request, agent.Agent, protocol.Command, string) (turnapi.Result, bool, error)
	Prompt   func(context.Context, turnapi.Request, agent.Agent, string) (turnapi.Result, error)
}

func Dispatch(ctx context.Context, req turnapi.Request, flow Flow) (turnapi.Result, error) {
	selected, prompt, switchOnly, err := flow.Select(req)
	if err != nil {
		return turnapi.Result{}, err
	}
	if switchOnly {
		return flow.Switched(selected), nil
	}
	prompt, command, rest, interrupt := flow.Parse(prompt)
	req.Admission.Queue = !interrupt
	if result, handled, err := flow.Control(ctx, req, selected, command, rest); handled {
		return result, err
	}
	return flow.Prompt(ctx, req, selected, prompt)
}
