package acphost

import (
	"context"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
)

func TestPermissionRequestPreservesExplicitToolKind(t *testing.T) {
	broker, _ := permission.New(permission.PolicyRead)
	wanted := acp.ToolKindExecute
	observed := false
	host := &Host{cfg: Config{Permission: broker}, collectors: map[acp.SessionID]*collector{"session": {generation: 1, ask: func(_ context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
		observed = true
		if ask.Kind != wanted {
			t.Errorf("explicit kind changed: got %q want %q", ask.Kind, wanted)
		}
		return permission.Choose(false, ask.Options), nil
	}}}}
	handler := clientHandler{h: host, generation: 1}
	response, err := handler.RequestPermission(t.Context(), &acp.RequestPermissionRequest{SessionID: "session", ToolCall: acp.ToolCallUpdate{ToolCallID: "tool", Kind: &wanted}, Options: []acp.PermissionOption{{OptionID: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce}}})
	if err != nil || !observed || response.Outcome.OptionID != "reject" {
		t.Fatalf("explicit permission kind did not reach broker: observed=%t response=%+v err=%v", observed, response, err)
	}
}
