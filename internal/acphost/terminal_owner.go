package acphost

import (
	"context"
	"runtime"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/procgroup"
)

const TerminalsSupported = runtime.GOOS == "linux" || runtime.GOOS == "darwin"

// TerminalIntent identifies one callback's original native owner, not an
// execution permit or a command that may be reconstructed after restart.
type TerminalIntent struct {
	ID         string
	SessionID  acp.SessionID
	Generation uint64
}

// TerminalOwner retains cleanup facts before either native helper gate opens.
// Admit must consume the payload only under fresh authenticated execution
// authority; preparing or retaining a terminal does not authorize its payload.
type TerminalOwner interface {
	Reserve(context.Context, TerminalIntent) error
	Prepared(context.Context, TerminalIntent, procgroup.Preparation, string) error
	Active(context.Context, TerminalIntent, procgroup.Identity, procgroup.Place) error
	Admit(context.Context, TerminalIntent, func(context.Context) error, func() error) error
	Stopped(context.Context, TerminalIntent) error
}
