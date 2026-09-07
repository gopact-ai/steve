package feishu

import (
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

func TestDecideAccess(t *testing.T) {
	open := Access{GroupPolicy: config.GroupPolicyOpen, Allowed: map[string]struct{}{}, Blocked: map[string]struct{}{}}
	allowlist := Access{
		GroupPolicy: config.GroupPolicyAllowlist,
		Allowed:     map[string]struct{}{"ou_user": {}},
		Blocked:     map[string]struct{}{},
	}
	blocked := Access{
		GroupPolicy: config.GroupPolicyOpen,
		Allowed:     map[string]struct{}{},
		Blocked:     map[string]struct{}{"ou_spam": {}},
	}
	disabled := Access{
		GroupPolicy: config.GroupPolicyDisabled,
		Allowed:     map[string]struct{}{"ou_user": {}},
		Blocked:     map[string]struct{}{},
	}

	tests := []struct {
		name   string
		access Access
		msg    InboundMessage
		want   string
	}{
		{"dm open", open, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_other"}, actionAllow},
		{"empty sender", open, InboundMessage{ChatType: "p2p"}, actionDrop},
		{"group open", open, InboundMessage{ChatType: "group", SenderOpenID: "ou_other"}, actionAllow},
		{"open ignores allowed list", Access{GroupPolicy: config.GroupPolicyOpen, Allowed: map[string]struct{}{"ou_user": {}}}, InboundMessage{ChatType: "group", SenderOpenID: "ou_other"}, actionAllow},
		{"empty allowlist denies everyone", Access{GroupPolicy: config.GroupPolicyAllowlist}, InboundMessage{ChatType: "group", SenderOpenID: "ou_user"}, actionDrop},
		{"blocked wins over allowlist", Access{GroupPolicy: config.GroupPolicyAllowlist, Allowed: map[string]struct{}{"ou_user": {}}, Blocked: map[string]struct{}{"ou_user": {}}}, InboundMessage{ChatType: "group", SenderOpenID: "ou_user"}, actionDrop},
		{"unknown group policy denies", Access{GroupPolicy: "unknown"}, InboundMessage{ChatType: "group", SenderOpenID: "ou_user"}, actionDrop},
		{"group allowlist hit", allowlist, InboundMessage{ChatType: "group", SenderOpenID: "ou_user"}, actionAllow},
		{"group allowlist miss", allowlist, InboundMessage{ChatType: "group", SenderOpenID: "ou_other"}, actionDrop},
		{"dm ignores allowlist", allowlist, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_other"}, actionAllow},
		{"blocked dm", blocked, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_spam"}, actionDrop},
		{"blocked group", blocked, InboundMessage{ChatType: "group", SenderOpenID: "ou_spam"}, actionDrop},
		{"disabled group", disabled, InboundMessage{ChatType: "group", SenderOpenID: "ou_user"}, actionDrop},
		{"disabled still allows dm", disabled, InboundMessage{ChatType: "p2p", SenderOpenID: "ou_user"}, actionAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decide(tt.msg, tt.access); got != tt.want {
				t.Fatalf("decide() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccessFrom(t *testing.T) {
	access := AccessFrom(config.Feishu{
		GroupPolicy:    config.GroupPolicyOpen,
		AllowedSenders: []string{" ou_user "},
		BlockedSenders: []string{"ou_spam"},
	})
	if _, ok := access.Allowed["ou_user"]; !ok {
		t.Fatalf("allowed = %#v", access.Allowed)
	}
	if _, ok := access.Blocked["ou_spam"]; !ok {
		t.Fatalf("blocked = %#v", access.Blocked)
	}
}
