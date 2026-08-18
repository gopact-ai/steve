package permission

import (
	"fmt"

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

func (b *Broker) Decide(kind acp.ToolKind, options []acp.PermissionOption) acp.RequestPermissionOutcome {
	pick := func(kinds ...acp.PermissionOptionKind) *acp.PermissionOption {
		for _, optionKind := range kinds {
			for i := range options {
				if options[i].Kind == optionKind {
					return &options[i]
				}
			}
		}
		return nil
	}
	allow := b.allows(kind)
	var option *acp.PermissionOption
	if !allow {
		option = pick(acp.PermissionOptionKindRejectAlways, acp.PermissionOptionKindRejectOnce)
	} else if b.policy == PolicyAlwaysAllow {
		option = pick(acp.PermissionOptionKindAllowAlways, acp.PermissionOptionKindAllowOnce)
	} else {
		option = pick(acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways)
	}
	if option == nil {
		return acp.CanceledRequestPermissionOutcome()
	}
	return acp.SelectedRequestPermissionOutcome(option.OptionID)
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
