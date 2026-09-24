package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/processrestart"
	"github.com/gopact-ai/steve/internal/skills"
)

// productionNode serves a node configured the way steve-node runs one:
// its sessions are authorized by the coordinator, and it restarts itself
// where the platform can re-execute it.
func productionNode(t *testing.T, cfg ServerConfig) *Server {
	t.Helper()
	cfg.SessionAuthorizer = CoordinatorSessionAuthorizer{}
	return startNode(t, cfg, func(s *Server) {
		if processrestart.Supported() {
			s.EnableRestart()
		}
	})
}

// Every operation a v2 node has is sent to it without looking for a
// feature: the protocol version settled at the handshake already says the
// node has them, so an advert listing no features is still served, and
// each operation is answered by the node itself.
func TestBaselineOperationsNeedNoAdvertisedFeature(t *testing.T) {
	bin := buildMockAgent(t)
	dir := t.TempDir()
	server := productionNode(t, ServerConfig{
		Name: "host-base", Token: "tok", StateDir: t.TempDir(), WorkspaceRoot: dir,
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}},
		Tools:     []string{"sh"},
	})
	registry := NewRegistry("hub-1", map[string]Config{"host-base": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	// The node asks the hub to authorize a session action or a receipt
	// before acting on it, so a question here is the request reaching it.
	var asked []string
	registry.SetSessionAuthorizer(func(_ context.Context, _ string, _ nodewire.SessionAuthority, _ nodewire.SessionBinding, action nodewire.SessionAction) error {
		asked = append(asked, "session "+string(action))
		return errors.New("refused by the test hub")
	})
	registry.SetNodeReceiptAuthorizer(func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionReceipt) error {
		asked = append(asked, "receipt")
		return errors.New("refused by the test hub")
	})
	c, err := registry.connect(t.Context(), "host-base")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "deploy")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := skills.Pack([]skills.Ref{{Name: "deploy", Path: src}})
	if err != nil {
		t.Fatal(err)
	}
	present, _ := ability.Compile([]string{"tool:sh"})
	made := filepath.Join(dir, "made-by-the-node")
	resume := nodeSessionRequest(nodewire.SessionActionOpen)
	resume.ID = "ns_" + strings.Repeat("0", 64)
	resume.Authority.ClusterID, resume.Binding.NodeID = "hub-1", "host-base"
	receipt := nodewire.SessionReceiptRequest{Authority: resume.Authority, Receipt: nodewire.SessionReceipt{
		Version: 1, SessionID: resume.ID, ContextID: "context-1", Binding: resume.Binding,
		CommandID: "command-1", InputSequence: 1, Digest: strings.Repeat("0", 64),
	}}

	for _, op := range []struct {
		name string
		// run fails unless the node itself answered.
		run func() error
	}{
		{"admission", func() error {
			adm, err := registry.Admit(t.Context(), "host-base", nodewire.AdmitRequest{Attempt: "a1", Harness: "codex", Requirement: present, Uses: []string{"absent"}})
			if err == nil && adm.Source != ability.SourceNode {
				err = fmt.Errorf("verdict from %q, want the node's own", adm.Source)
			}
			return err
		}},
		{"inspect", func() error {
			_, err := registry.Inspect(t.Context(), "host-base", dir)
			return err
		}},
		{"MCP probe", func() error {
			_, err := registry.MCPProbe(t.Context(), "host-base", "absent")
			return err
		}},
		{"configure", func() error {
			settings, err := registry.Settings(t.Context(), "host-base")
			if err == nil {
				_, err = registry.Configure(t.Context(), "host-base", settings)
			}
			return err
		}},
		{"push skills", func() error {
			if err := registry.PushSkills(t.Context(), "host-base", bundle); err != nil {
				return err
			}
			if got := server.currentSkills(); got != bundle.Hash {
				return fmt.Errorf("node holds skills %q, want %q", got, bundle.Hash)
			}
			return nil
		}},
		{"files", func() error {
			if _, err := registry.Files(t.Context(), "host-base", nodewire.FileRequest{Op: nodewire.FileMkdir, Path: made}); err != nil {
				return err
			}
			if info, err := os.Stat(made); err != nil || !info.IsDir() {
				return fmt.Errorf("the node made no directory: %v", err)
			}
			return nil
		}},
		{"artifact", func() error {
			// Only the node validates an artifact request, so a typed
			// refusal of an empty one is the node's answer.
			var refused *nodewire.OperationFailure
			if _, err := registry.Artifact(t.Context(), "host-base", nodewire.ArtifactRequest{}); !errors.As(err, &refused) || refused.Code != "invalid_request" {
				return fmt.Errorf("%v, want the node's refusal of an empty request", err)
			}
			return nil
		}},
		{"agent tools", func() error {
			discovery, err := registry.AgentTools(t.Context(), "host-base")
			if err == nil && discovery.Revision == "" {
				err = errors.New("no discovery revision")
			}
			return err
		}},
		{"native resume", func() error {
			var unsent *nodewire.SessionNotDispatched
			if _, err := registry.NodeSession(t.Context(), "host-base", resume); err == nil || errors.As(err, &unsent) || !slices.Contains(asked, "session open") {
				return fmt.Errorf("%v (node asked %v), want the node to ask for the authority the hub refuses", err, asked)
			}
			return nil
		}},
		{"receipt", func() error {
			if err := registry.AcknowledgeNodeReceipt(t.Context(), "host-base", receipt); err == nil || !slices.Contains(asked, "receipt") {
				return fmt.Errorf("%v (node asked %v), want the node to ask for the proof the hub refuses", err, asked)
			}
			return nil
		}},
	} {
		// A refresh after an earlier operation brings the node's features
		// back; each operation starts from an advert that lists none.
		advert := c.getAdvert()
		advert.Features = nil
		c.setAdvert(advert)
		if err := op.run(); err != nil {
			t.Errorf("%s: %v", op.name, err)
		}
	}
}

// An advert lists only what a v2 node may lack and this one has: native
// history where the platform can store it, and restart where the node can
// re-execute itself. The baseline is the protocol version, not a list, and
// the snapshot repeats no features.
func TestAdvertListsOnlyConditionalFeatures(t *testing.T) {
	server := productionNode(t, ServerConfig{Name: "host-list", Token: "tok", StateDir: t.TempDir()})
	registry := NewRegistry("hub-1", map[string]Config{"host-list": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	advert, err := registry.Advert(t.Context(), "host-list")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if nativehistory.StorageSupported {
		want = append(want, nodewire.FeatureNativeHistory)
	}
	if processrestart.Supported() {
		want = append(want, nodewire.FeatureRestart)
	}
	if got := slices.Sorted(slices.Values(advert.Features)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("advert lists %v, want %v", got, want)
	}
	raw, _ := json.Marshal(advert.Snapshot)
	if strings.Contains(string(raw), `"features"`) {
		t.Errorf("snapshot = %s, want no feature list", raw)
	}
}
