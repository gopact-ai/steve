package feishu

import (
	"maps"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/protocol"
)

const (
	actionAllow = "allow"
	actionDrop  = "drop"
)

type Access struct {
	GroupPolicy string
	Allowed     map[string]struct{}
	Blocked     map[string]struct{}
}

// accessPolicy is immutable after publication. Mention filtering and sender
// authorization must use the same snapshot for each incoming message.
type accessPolicy struct {
	access           Access
	allowUnmentioned bool
}

// SetAccess changes admission for subsequent incoming messages only. It neither
// replays rejected messages nor interrupts messages already being accepted.
// The channel owns a copy of the maps; callers may reuse them after this returns.
func (c *Channel) SetAccess(access Access, allowUnmentioned bool) {
	access.Allowed = maps.Clone(access.Allowed)
	access.Blocked = maps.Clone(access.Blocked)
	c.policy.Store(&accessPolicy{access: access, allowUnmentioned: allowUnmentioned})
}

func (c *Channel) loadAccess(allowUnmentioned bool) accessPolicy {
	if policy := c.policy.Load(); policy != nil {
		return *policy
	}
	// Preserve the construction-time fallback for literal channels in tests.
	return accessPolicy{access: c.access, allowUnmentioned: allowUnmentioned}
}

func AccessFrom(cfg config.Feishu) Access {
	return Access{
		GroupPolicy: cfg.GroupPolicy,
		Allowed:     idSet(cfg.AllowedSenders),
		Blocked:     idSet(cfg.BlockedSenders),
	}
}

func decide(msg InboundMessage, access Access) string {
	if msg.SenderOpenID == "" {
		return actionDrop
	}
	if _, blocked := access.Blocked[msg.SenderOpenID]; blocked {
		return actionDrop
	}
	if msg.ChatType != protocol.ChatGroup {
		return actionAllow
	}
	switch access.GroupPolicy {
	case config.GroupPolicyOpen:
		return actionAllow
	case config.GroupPolicyAllowlist:
		if _, ok := access.Allowed[msg.SenderOpenID]; ok {
			return actionAllow
		}
	}
	return actionDrop
}

func idSet(ids []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}
