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
		expected acp.PermissionOptionID
	}{
		{name: "auto", policy: "auto", expected: "allow"},
		{name: "always allow", policy: "always_allow", expected: "allow"},
		{name: "deny", policy: "deny", expected: "reject"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker, err := New(tt.policy)
			if err != nil {
				t.Fatal(err)
			}
			outcome := broker.Decide(options)
			if outcome.OptionID != tt.expected {
				t.Fatalf("unexpected option: %q", outcome.OptionID)
			}
		})
	}
}

func TestBrokerDenyPrefersRejectAlways(t *testing.T) {
	broker, err := New("deny")
	if err != nil {
		t.Fatal(err)
	}
	outcome := broker.Decide([]acp.PermissionOption{
		{OptionID: "reject-once", Kind: acp.PermissionOptionKindRejectOnce},
		{OptionID: "reject-always", Kind: acp.PermissionOptionKindRejectAlways},
	})
	if outcome.OptionID != "reject-always" {
		t.Fatalf("expected reject-always, got %q", outcome.OptionID)
	}
}

func TestBrokerDenyWithoutRejectOptionCancels(t *testing.T) {
	broker, err := New("deny")
	if err != nil {
		t.Fatal(err)
	}
	outcome := broker.Decide([]acp.PermissionOption{
		{OptionID: "allow", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionID: "always", Kind: acp.PermissionOptionKindAllowAlways},
	})
	if outcome.Outcome != acp.RequestPermissionOutcomeTypeCanceled {
		t.Fatalf("expected canceled outcome, got %q", outcome.Outcome)
	}
}
