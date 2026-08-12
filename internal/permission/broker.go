package permission

import (
	"fmt"

	"github.com/gopact-ai/acp"
)

type Broker struct{ policy string }

func New(policy string) (*Broker, error) {
	switch policy {
	case "auto", "always_allow", "deny":
		return &Broker{policy: policy}, nil
	default:
		return nil, fmt.Errorf("unknown permission policy %q", policy)
	}
}

func (b *Broker) Decide(options []acp.PermissionOption) acp.RequestPermissionOutcome {
	pick := func(kinds ...acp.PermissionOptionKind) *acp.PermissionOption {
		for _, kind := range kinds {
			for i := range options {
				if options[i].Kind == kind {
					return &options[i]
				}
			}
		}
		return nil
	}
	var option *acp.PermissionOption
	switch b.policy {
	case "deny":
		// Prefer reject_always so the agent cannot retry the same tool in a
		// request_permission loop.
		option = pick(acp.PermissionOptionKindRejectAlways, acp.PermissionOptionKindRejectOnce)
	case "always_allow":
		option = pick(acp.PermissionOptionKindAllowAlways, acp.PermissionOptionKindAllowOnce)
	default:
		option = pick(acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways)
	}
	if option == nil {
		return acp.CanceledRequestPermissionOutcome()
	}
	return acp.SelectedRequestPermissionOutcome(option.OptionID)
}
