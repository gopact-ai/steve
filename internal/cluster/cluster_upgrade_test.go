package cluster

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/coordination"
)

// Only a member this node keeps a link to can be upgraded from here, over
// that link's alias and with the program bundled for the member's platform;
// this node itself is not.
func TestUpgradeTargetNamesTheLinkAliasAndRefusesSelf(t *testing.T) {
	peer := &Peer{Config: PeerConfig{NodeID: "node-hub", Links: map[string]PeerLink{"node-dev": {Alias: "dev", Remote: coordination.Route{Raft: "127.0.0.1:25407", API: "127.0.0.1:25408"}}}}}
	backend := peerSSHBackend{peer: peer, findBinary: func(platform string) (string, bool) { return "/bundle/" + platform, platform == "linux/amd64" }}
	target, err := backend.UpgradeTarget(t.Context(), "node-dev")
	if err != nil || target.Alias != "dev" || target.Version == "" {
		t.Fatalf("target = %#v %v", target, err)
	}
	if path, ok := target.FindBinary("linux/amd64"); !ok || path != "/bundle/linux/amd64" {
		t.Fatalf("binary lookup = %q %v", path, ok)
	}
	if _, err := backend.UpgradeTarget(t.Context(), "node-hub"); err == nil || !strings.Contains(err.Error(), "本机") {
		t.Fatalf("self was not refused: %v", err)
	}
	if _, err := backend.UpgradeTarget(t.Context(), "node-other"); err == nil || !strings.Contains(err.Error(), "隧道") {
		t.Fatalf("a member without a link was not refused: %v", err)
	}
}
