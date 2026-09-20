package turn

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/state"
)

type continuityGate struct {
	fakeGate
	denied   string
	prepared []string
	failure  error
}

func (g *continuityGate) DescribeExtras(token, endpoint string) []capability.Extra {
	extras := g.fakeGate.DescribeExtras(token, endpoint)
	extras[0].Name = "steve"
	return extras
}

func (g *continuityGate) PrepareExtras(conversation, agent, token, endpoint string) ([]capability.Extra, error) {
	g.prepared = append(g.prepared, token)
	if g.failure != nil {
		return nil, g.failure
	}
	if token == g.denied {
		return nil, agentmcp.ErrGrantDenied
	}
	return g.DescribeExtras(token, endpoint), nil
}

func credentialTurn(t *testing.T) (*chatTurn, *continuityGate, state.Session) {
	t.Helper()
	c, _, _, _, _ := gateCoordinator(t, true)
	g := &continuityGate{denied: "revoked-token"}
	c.gate = g
	req := Request{ConversationID: "chat", Input: "continue", MessageID: "next"}
	selected := c.catalog.Default()
	binding, workspace, err := c.resolveWorkspace(t.Context(), req, selected)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := c.assemble(selected, req, g.DescribeExtras(g.denied, ""))
	if err != nil {
		t.Fatal(err)
	}
	saved := state.Session{ConversationID: "chat", AgentID: selected.ID, HarnessID: selected.Harness,
		UpstreamID: "ns_original", Workspace: workspace.Path, ProjectID: binding.ProjectID, ProjectVersion: binding.Version,
		AgentToken: g.denied, CapabilityHash: caps.Fingerprint, SessionConfigHash: caps.SessionFingerprint, InstructionsApplied: true}
	if err := c.store.SaveSession(saved); err != nil {
		t.Fatal(err)
	}
	return &chatTurn{c: c, req: req, selected: selected, binding: binding, workspace: workspace, clock: newTurnClock()}, g, saved
}

func TestRevokedCredentialRefreshKeepsNativeContextAndDurableRetryToken(t *testing.T) {
	turn, g, before := credentialTurn(t)
	if err := turn.prepareSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if turn.saved.UpstreamID != before.UpstreamID || turn.saved.AgentToken == before.AgentToken || turn.saved.AgentToken == "" {
		t.Fatal("credential renewal replaced context or reused revoked credential")
	}
	pending := turn.c.store.Conversation("chat").Sessions[turn.selected.ID]
	if pending.AgentToken != before.AgentToken || pending.PendingAgentToken != turn.saved.AgentToken || pending.CapabilityHash != before.CapabilityHash {
		t.Fatal("credential proof or durable retry token lost before node resume")
	}
	if turn.credentialRefresh == nil || turn.credentialRefresh.PreviousAuthorization != "Bearer "+before.AgentToken {
		t.Fatal("node did not receive exact previous-configuration proof")
	}
	if len(turn.c.store.Conversation("chat").Archived) != 0 {
		t.Fatal("rotation archived native context")
	}
	// A retry after a failed open/crash must not generate a third credential.
	retry := *turn
	retry.clock = newTurnClock()
	if err := retry.prepareSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	if retry.saved.AgentToken != turn.saved.AgentToken {
		t.Fatal("retry lost pending credential")
	}
	g.denied = ""
}

func TestCredentialRefreshDoesNotHideDriftOrStorageFailure(t *testing.T) {
	for _, mode := range []string{"config", "workspace", "storage", "unmanaged", "harness"} {
		t.Run(mode, func(t *testing.T) {
			turn, g, saved := credentialTurn(t)
			switch mode {
			case "config":
				saved.SessionConfigHash = "unrelated"
				saved.CapabilityHash = "unrelated"
			case "workspace":
				turn.workspace.Path = "/different"
			case "storage":
				g.failure = errors.New("store unavailable")
			case "unmanaged":
				saved.UpstreamID = "unmanaged-native"
			case "harness":
				turn.selected.Harness = "different"
			}
			if err := turn.c.store.SaveSession(saved); err != nil {
				t.Fatal(err)
			}
			if err := turn.prepareSession(t.Context()); err == nil {
				t.Fatal("unsafe refresh was accepted")
			}
			if after := turn.c.store.Conversation("chat").Sessions[turn.selected.ID]; !reflect.DeepEqual(saved, after) {
				t.Fatal("failed verification changed retained session")
			}
			for _, token := range g.prepared {
				if token != saved.AgentToken {
					t.Fatal("replacement prepared before verification")
				}
			}
		})
	}
}
