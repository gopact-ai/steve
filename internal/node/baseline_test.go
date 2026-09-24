package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ability"
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

// An advert lists only what a v2 node may lack; the baseline is the
// protocol version, not a list, and the snapshot repeats no features.
func TestAdvertListsOnlyConditionalFeatures(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "host-list", Token: "tok", StateDir: t.TempDir()})
	registry := NewRegistry("hub-1", map[string]Config{"host-list": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	advert, err := registry.Advert(t.Context(), "host-list")
	if err != nil {
		t.Fatal(err)
	}
	conditional := []string{nodewire.FeatureNodeSessions, nodewire.FeatureNativeResume, nodewire.FeatureNodeReceipts, nodewire.FeatureNativeHistory, nodewire.FeatureRestart}
	for _, feature := range advert.Features {
		if !slices.Contains(conditional, feature) {
			t.Errorf("advert lists %q, which every v2 node has", feature)
		}
	}
	raw, _ := json.Marshal(advert.Snapshot)
	if strings.Contains(string(raw), `"features"`) {
		t.Errorf("snapshot = %s, want no feature list", raw)
	}
}
