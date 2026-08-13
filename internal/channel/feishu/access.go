package feishu

import "github.com/gopact-ai/steve/internal/config"

const (
	actionAllow   = "allow"
	actionDrop    = "drop"
	actionPairing = "pairing"
)

type Access struct {
	DMPolicy         string
	GroupPolicy      string
	AllowUnmentioned bool
	Allowed          map[string]struct{}
}

func AccessFrom(cfg config.Feishu, extra []string) Access {
	allowed := make(map[string]struct{}, len(cfg.AllowedSenders)+len(extra))
	for _, sender := range cfg.AllowedSenders {
		allowed[sender] = struct{}{}
	}
	for _, sender := range extra {
		allowed[sender] = struct{}{}
	}
	return Access{
		DMPolicy:         cfg.DMPolicy,
		GroupPolicy:      cfg.GroupPolicy,
		AllowUnmentioned: cfg.AllowUnmentioned,
		Allowed:          allowed,
	}
}

func (a Access) allows(openID string, extra func(string) bool) bool {
	if openID == "" {
		return false
	}
	if _, ok := a.Allowed[openID]; ok {
		return true
	}
	return extra != nil && extra(openID)
}

func decide(msg InboundMessage, access Access, extra func(string) bool) string {
	if access.allows(msg.SenderOpenID, extra) {
		if msg.ChatType == "group" && access.GroupPolicy == config.GroupPolicyDisabled {
			return actionDrop
		}
		return actionAllow
	}
	if msg.ChatType == "group" {
		switch access.GroupPolicy {
		case config.GroupPolicyOpen:
			return actionAllow
		default:
			return actionDrop
		}
	}
	if access.DMPolicy == config.DMPolicyPairing && msg.SenderOpenID != "" {
		return actionPairing
	}
	return actionDrop
}
