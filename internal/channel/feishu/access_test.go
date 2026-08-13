package feishu

import (
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

func TestDecideAccess(t *testing.T) {
	allowed := Access{
		DMPolicy:    config.DMPolicyAllowlist,
		GroupPolicy: config.GroupPolicyAllowlist,
		Allowed:     map[string]struct{}{"ou_user": {}},
	}
	pairing := Access{
		DMPolicy:    config.DMPolicyPairing,
		GroupPolicy: config.GroupPolicyAllowlist,
		Allowed:     map[string]struct{}{"ou_user": {}},
	}
	openGroup := Access{
		DMPolicy:    config.DMPolicyPairing,
		GroupPolicy: config.GroupPolicyOpen,
		Allowed:     map[string]struct{}{},
	}
	disabledGroup := Access{
		DMPolicy:    config.DMPolicyAllowlist,
		GroupPolicy: config.GroupPolicyDisabled,
		Allowed:     map[string]struct{}{"ou_user": {}},
	}

	tests := []struct {
		name   string
		access Access
		msg    InboundMessage
		want   string
	}{
		{"allowlisted dm", allowed, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_user"}, actionAllow},
		{"unknown dm allowlist", allowed, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_other"}, actionDrop},
		{"unknown dm pairing", pairing, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_other"}, actionPairing},
		{"known dm pairing", pairing, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_user"}, actionAllow},
		{"allowlisted group", allowed, InboundMessage{ChatType: "group", SenderOpenID: "ou_user"}, actionAllow},
		{"unknown group allowlist", allowed, InboundMessage{ChatType: "group", SenderOpenID: "ou_other"}, actionDrop},
		{"unknown group open", openGroup, InboundMessage{ChatType: "group", SenderOpenID: "ou_other"}, actionAllow},
		{"unknown dm with open groups", openGroup, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_other"}, actionPairing},
		{"disabled group even if allowed", disabledGroup, InboundMessage{ChatType: "group", SenderOpenID: "ou_user"}, actionDrop},
	}
	if got := decide(InboundMessage{ChatType: "p2p", SenderOpenID: "ou_live"}, allowed, func(id string) bool { return id == "ou_live" }); got != actionAllow {
		t.Fatalf("extra allow = %q", got)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decide(tt.msg, tt.access, nil); got != tt.want {
				t.Fatalf("decide() = %q, want %q", got, tt.want)
			}
		})
	}
}
