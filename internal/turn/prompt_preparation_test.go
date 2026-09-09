package turn

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
)

// Multiple simultaneous drifts must report the same first refusal, before
// opening a session. Rejection must still release the conversation's turn.
func TestPromptPreparationRefusalPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                string
		tainted, capability bool
		want                i18n.Key
	}{
		{"tainted-before-capability-and-workspace", true, true, i18n.Tainted},
		{"capability-before-workspace", false, true, i18n.CapabilityDrift},
		{"workspace", false, false, i18n.WorkspaceDrift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
			if err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "done"}}}
			c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
			req := Request{ConversationID: "chat", Input: "hello"}
			caps, err := c.assemble(catalog.Default(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			saved := state.Session{ConversationID: "chat", AgentID: "codex", HarnessID: "codex", UpstreamID: "saved", Workspace: "different", CapabilityHash: caps.Fingerprint, Tainted: tc.tainted}
			if tc.capability {
				saved.CapabilityHash = "old"
			}
			if err := store.SaveSession(saved); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				_, err := c.prompt(t.Context(), req, catalog.Default(), "hello")
				if err == nil || err.Error() != c.text.T(tc.want, protocol.CommandNew) {
					t.Fatalf("refusal = %v", err)
				}
				if len(manager.opened) != 0 {
					t.Fatalf("rejected turn opened %v", manager.opened)
				}
			}
		})
	}
}
