package permission

import (
	"context"
	"fmt"
	"strings"

	"github.com/gopact-ai/acp"
)

const (
	PolicyAuto        = "auto"
	PolicyAlwaysAllow = "always_allow"
	PolicyDeny        = "deny"
	PolicyRead        = "read"
	PolicyWrite       = "write"
)

type Broker struct{ policy string }

func New(policy string) (*Broker, error) {
	switch policy {
	case PolicyAuto, PolicyAlwaysAllow, PolicyDeny, PolicyRead, PolicyWrite:
		return &Broker{policy: policy}, nil
	default:
		return nil, fmt.Errorf("unknown permission policy %q", policy)
	}
}

// Ask is one tool-permission prompt that must be answered by a human.
type Ask struct {
	SessionID  string
	Generation uint64
	ToolCallID string
	ToolName   string
	Kind       acp.ToolKind
	Reason     string
	Options    []acp.PermissionOption
}

type AskFunc func(context.Context, Ask) (acp.RequestPermissionOutcome, error)

func (b *Broker) Decide(kind acp.ToolKind, options []acp.PermissionOption) acp.RequestPermissionOutcome {
	if !b.allows(kind) {
		return pickOutcome(options, acp.PermissionOptionKindRejectAlways, acp.PermissionOptionKindRejectOnce)
	}
	if b.policy == PolicyAlwaysAllow {
		return pickOutcome(options, acp.PermissionOptionKindAllowAlways, acp.PermissionOptionKindAllowOnce)
	}
	return pickOutcome(options, acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways)
}

// Allow reports whether the policy allows this tool kind outright.
func (b *Broker) Allow(kind acp.ToolKind) bool { return b.allows(kind) }

// NeedsAsk reports whether a human must confirm this tool kind.
// read: reads pass, writes wait. Other policies decide without asking.
func (b *Broker) NeedsAsk(kind acp.ToolKind) bool {
	return b != nil && b.policy == PolicyRead && !isRead(kind)
}

// SessionMode picks the agent session mode that matches the policy. Agents
// only ask for permission in a restricted mode; left in their default mode
// they approve their own writes and the policy never applies. Mode IDs are
// agent-defined, so match on the words they all seem to use and return ""
// when nothing fits, which leaves the agent's default alone.
func (b *Broker) SessionMode(available []string) string {
	if b == nil || len(available) == 0 {
		return ""
	}
	var wanted []string
	switch b.policy {
	case PolicyRead, PolicyDeny:
		wanted = []string{"read-only", "readonly", "read", "plan"}
	case PolicyAlwaysAllow:
		wanted = []string{"full-access", "bypass", "yolo"}
	default:
		return ""
	}
	for _, want := range wanted {
		for _, id := range available {
			if strings.EqualFold(id, want) || strings.Contains(strings.ToLower(id), want) {
				return id
			}
		}
	}
	return ""
}

// Choose maps a human allow/deny to an ACP option. Fail-closed when no match.
func Choose(allow bool, options []acp.PermissionOption) acp.RequestPermissionOutcome {
	if allow {
		return pickOutcome(options, acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways)
	}
	return pickOutcome(options, acp.PermissionOptionKindRejectOnce, acp.PermissionOptionKindRejectAlways)
}

func pickOutcome(options []acp.PermissionOption, kinds ...acp.PermissionOptionKind) acp.RequestPermissionOutcome {
	for _, optionKind := range kinds {
		for i := range options {
			if options[i].Kind == optionKind {
				return acp.SelectedRequestPermissionOutcome(options[i].OptionID)
			}
		}
	}
	return acp.CanceledRequestPermissionOutcome()
}

func (b *Broker) allows(kind acp.ToolKind) bool {
	switch b.policy {
	case PolicyDeny:
		return false
	case PolicyRead:
		return isRead(kind)
	default:
		return true
	}
}

func isRead(kind acp.ToolKind) bool {
	switch kind {
	case acp.ToolKindRead, acp.ToolKindSearch, acp.ToolKindFetch, acp.ToolKindThink:
		return true
	default:
		return false
	}
}
