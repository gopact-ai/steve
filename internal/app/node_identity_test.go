package app

import (
	"os"
	"strings"
	"sync"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/cluster"
)

func TestLocalNodeNamePrefersTheClusterIdentity(t *testing.T) {
	t.Setenv("STEVE_NODE", "configured")
	if got := localNodeName(&Environment{NodeID: "node-a"}); got != "node-a" {
		t.Fatalf("cluster member named %q", got)
	}
	if got := localNodeName(&Environment{}); got != "configured" {
		t.Fatalf("environment without identity named %q", got)
	}
	if got := localNodeName(nil); got != "configured" {
		t.Fatalf("standalone named %q", got)
	}
	t.Setenv("STEVE_NODE", " ")
	want, err := os.Hostname()
	if err != nil || want == "" {
		want = "local"
	}
	if got := localNodeName(nil); got != want {
		t.Fatalf("standalone without STEVE_NODE named %q, want %q", got, want)
	}
}

// The administration service is not built for a machine without a name.
func TestConsoleAssemblyRefusesAnUnnamedNode(t *testing.T) {
	_, err := assembleConsole(&applicationLifetime{}, &assemblyInput{}, &runtimeValues{}, nil, nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "node name") {
		t.Fatalf("assembled without a node name: %v", err)
	}
}

// A cluster member's application administers the member it runs on: the
// activation's node identity reaches the administration service, ahead of
// STEVE_NODE.
func TestPeerApplicationAdministersItsClusterNode(t *testing.T) {
	t.Setenv("STEVE_NODE", "not-the-member")
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var mu sync.Mutex
	var named, activated string
	options.ApplicationReady = func(a *adminsvc.Service, _ cluster.ApplicationServer, activation cluster.Activation) error {
		mu.Lock()
		defer mu.Unlock()
		named, activated = a.NodeName, activation.NodeID
		return nil
	}
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	mu.Lock()
	defer mu.Unlock()
	if activated == "" || activated != peer.Config.NodeID || named != activated {
		t.Fatalf("administration service names %q; activation %q, member %q", named, activated, peer.Config.NodeID)
	}
}
