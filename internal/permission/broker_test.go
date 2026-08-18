package permission

import (
	"testing"

	"github.com/gopact-ai/acp"
)

func TestBrokerDecide(t *testing.T) {
	options := []acp.PermissionOption{
		{OptionID: "allow", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionID: "reject", Kind: acp.PermissionOptionKindRejectOnce},
	}
	tests := []struct {
		name     string
		policy   string
		kind     acp.ToolKind
		expected acp.PermissionOptionID
	}{
		{name: "auto", policy: PolicyAuto, kind: acp.ToolKindExecute, expected: "allow"},
		{name: "always allow", policy: PolicyAlwaysAllow, kind: acp.ToolKindExecute, expected: "allow"},
		{name: "deny", policy: PolicyDeny, kind: acp.ToolKindRead, expected: "reject"},
		{name: "read allows read", policy: PolicyRead, kind: acp.ToolKindRead, expected: "allow"},
		{name: "read allows search", policy: PolicyRead, kind: acp.ToolKindSearch, expected: "allow"},
		{name: "read denies edit", policy: PolicyRead, kind: acp.ToolKindEdit, expected: "reject"},
		{name: "read denies execute", policy: PolicyRead, kind: acp.ToolKindExecute, expected: "reject"},
		{name: "read denies unknown", policy: PolicyRead, expected: "reject"},
		{name: "write allows edit", policy: PolicyWrite, kind: acp.ToolKindEdit, expected: "allow"},
		{name: "write allows read", policy: PolicyWrite, kind: acp.ToolKindRead, expected: "allow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker, err := New(tt.policy)
			if err != nil {
				t.Fatal(err)
			}
			outcome := broker.Decide(tt.kind, options)
			if outcome.OptionID != tt.expected {
				t.Fatalf("unexpected option: %q", outcome.OptionID)
			}
		})
	}
}

func TestBrokerDenyPrefersRejectAlways(t *testing.T) {
	broker, err := New(PolicyDeny)
	if err != nil {
		t.Fatal(err)
	}
	outcome := broker.Decide(acp.ToolKindRead, []acp.PermissionOption{
		{OptionID: "reject-once", Kind: acp.PermissionOptionKindRejectOnce},
		{OptionID: "reject-always", Kind: acp.PermissionOptionKindRejectAlways},
	})
	if outcome.OptionID != "reject-always" {
		t.Fatalf("expected reject-always, got %q", outcome.OptionID)
	}
}

func TestBrokerDenyWithoutRejectOptionCancels(t *testing.T) {
	broker, err := New(PolicyRead)
	if err != nil {
		t.Fatal(err)
	}
	outcome := broker.Decide(acp.ToolKindExecute, []acp.PermissionOption{
		{OptionID: "allow", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionID: "always", Kind: acp.PermissionOptionKindAllowAlways},
	})
	if outcome.Outcome != acp.RequestPermissionOutcomeTypeCanceled {
		t.Fatalf("expected canceled outcome, got %q", outcome.Outcome)
	}
}

func TestBrokerRejectsUnknownPolicy(t *testing.T) {
	if _, err := New("maybe"); err == nil {
		t.Fatal("expected unknown policy error")
	}
}
