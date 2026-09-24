package node

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/skills"
)

// Every operation a v2 node has is sent to it without looking for a
// feature: the protocol version settled at the handshake already says the
// node has them, so an advert listing no features is still served.
func TestBaselineOperationsNeedNoAdvertisedFeature(t *testing.T) {
	bin := buildMockAgent(t)
	dir := t.TempDir()
	server := startNode(t, ServerConfig{
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
	advert := c.getAdvert()
	advert.Features = nil
	c.setAdvert(advert)

	present, _ := ability.Compile([]string{"tool:sh"})
	adm, err := registry.Admit(t.Context(), "host-base", nodewire.AdmitRequest{Attempt: "a1", Harness: "codex", Requirement: present, Uses: []string{"absent"}})
	if err != nil || adm.Source != ability.SourceNode {
		t.Errorf("admission = %+v, %v; want the node's own verdict", adm, err)
	}
	if _, err := registry.Inspect(t.Context(), "host-base", dir); err != nil {
		t.Errorf("inspect: %v", err)
	}
	if _, err := registry.MCPProbe(t.Context(), "host-base", "absent"); err != nil {
		t.Errorf("probe: %v", err)
	}
	if settings, err := registry.Settings(t.Context(), "host-base"); err != nil {
		t.Errorf("settings: %v", err)
	} else if _, err := registry.Configure(t.Context(), "host-base", settings); err != nil {
		t.Errorf("configure: %v", err)
	}
	if err := registry.PushSkills(t.Context(), "host-base", skills.Bundle{Hash: c.getAdvert().Skills}); err != nil {
		t.Errorf("push skills: %v", err)
	}
	if _, err := registry.Files(t.Context(), "host-base", nodewire.FileRequest{}); err != nil && strings.Contains(err.Error(), "needs an upgrade") {
		t.Errorf("files: %v", err)
	}
	if _, err := registry.Artifact(t.Context(), "host-base", nodewire.ArtifactRequest{}); err != nil && strings.Contains(err.Error(), "needs an upgrade") {
		t.Errorf("artifact: %v", err)
	}
	if _, err := registry.AgentTools(t.Context(), "host-base"); err != nil && strings.Contains(err.Error(), "upgrade") {
		t.Errorf("agent tools: %v", err)
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
