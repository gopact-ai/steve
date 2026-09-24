package consoleapi

import (
	"context"

	"github.com/gopact-ai/steve/internal/agenttools"
)

// NodeAgentService lists the coding tools installed on a machine and enrolls
// agents that run them.
type NodeAgentService interface {
	NodeAgents(context.Context, string) (agenttools.Discovery, error)
	EnrollNodeAgent(context.Context, string, agenttools.EnrollRequest) (agenttools.Enrollment, error)
}
