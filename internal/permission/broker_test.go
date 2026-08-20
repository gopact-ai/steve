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

func TestBrokerNeedsAsk(t *testing.T) {
	read, err := New(PolicyRead)
	if err != nil {
		t.Fatal(err)
	}
	if !read.NeedsAsk(acp.ToolKindEdit) || !read.NeedsAsk(acp.ToolKindExecute) {
		t.Fatal("read policy should ask for writes")
	}
	if read.NeedsAsk(acp.ToolKindRead) || read.NeedsAsk(acp.ToolKindSearch) {
		t.Fatal("read policy should not ask for reads")
	}
	write, err := New(PolicyWrite)
	if err != nil {
		t.Fatal(err)
	}
	if write.NeedsAsk(acp.ToolKindEdit) {
		t.Fatal("write policy should auto-allow edits")
	}
}

func TestChoosePrefersOnceOptions(t *testing.T) {
	options := []acp.PermissionOption{
		{OptionID: "always", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionID: "allow", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionID: "reject-always", Kind: acp.PermissionOptionKindRejectAlways},
		{OptionID: "reject", Kind: acp.PermissionOptionKindRejectOnce},
	}
	if got := Choose(true, options); got.OptionID != "allow" {
		t.Fatalf("allow = %q", got.OptionID)
	}
	if got := Choose(false, options); got.OptionID != "reject" {
		t.Fatalf("deny = %q", got.OptionID)
	}
}

func TestBrokerSessionMode(t *testing.T) {
	codex := []string{"read-only", "agent", "agent-full-access"}
	claude := []string{"default", "acceptEdits", "bypassPermissions", "plan"}
	tests := []struct {
		name      string
		policy    string
		available []string
		expected  string
	}{
		{name: "read picks read-only", policy: PolicyRead, available: codex, expected: "read-only"},
		{name: "deny picks read-only", policy: PolicyDeny, available: codex, expected: "read-only"},
		{name: "always allow picks full access", policy: PolicyAlwaysAllow, available: codex, expected: "agent-full-access"},
		{name: "write keeps default", policy: PolicyWrite, available: codex, expected: ""},
		{name: "read falls back to plan", policy: PolicyRead, available: claude, expected: "plan"},
		{name: "always allow picks bypass", policy: PolicyAlwaysAllow, available: claude, expected: "bypassPermissions"},
		{name: "no modes", policy: PolicyRead, expected: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker, err := New(tt.policy)
			if err != nil {
				t.Fatal(err)
			}
			if got := broker.SessionMode(tt.available); got != tt.expected {
				t.Fatalf("mode = %q, want %q", got, tt.expected)
			}
		})
	}
}
