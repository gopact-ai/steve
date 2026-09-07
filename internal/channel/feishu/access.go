package feishu

import (
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
