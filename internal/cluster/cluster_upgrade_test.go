package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/sshconnect"
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

// The wait asks the machine itself, over the link, which build it runs, and
// keeps asking until that is the wanted one: an ask that hangs is given up on
// so the next one reaches the restarted process, and running out of time names
// what the machine last reported.
func TestAwaitBuildAsksTheMachineUntilItReportsTheBuild(t *testing.T) {
	asks := 0
	ask := func(ctx context.Context) (string, error) {
		asks++
		switch asks {
		case 1:
			return "", errors.New("connection reset")
		case 2:
			<-ctx.Done()
			return "", ctx.Err()
		case 3:
			return "old1234", nil
		}
		return "new5678", nil
	}
	var reported []string
	ctx := sshconnect.WithReporter(t.Context(), func(text string) { reported = append(reported, text) })
	if err := awaitBuildWithin(ctx, ask, 3*time.Second, 50*time.Millisecond, "new5678"); err != nil {
		t.Fatalf("the machine's new build was not accepted: %v (asks=%d)", err, asks)
	}
	if asks != 4 || len(reported) != 2 || !strings.Contains(reported[0], "connection reset") || !strings.Contains(reported[1], "重新询问") {
		t.Fatalf("asks=%d reported=%v", asks, reported)
	}
	stale := func(context.Context) (string, error) { return "old1234", nil }
	short, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	err := awaitBuildWithin(short, stale, time.Second, 50*time.Millisecond, "new5678")
	if err == nil || !strings.Contains(err.Error(), "old1234") {
		t.Fatalf("running out of time did not name the version seen: %v", err)
	}
	// A machine still on a build that reports no version at all answers, so
	// the wait must not claim it never did.
	silent := func(context.Context) (string, error) { return "", nil }
	short2, cancel2 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel2()
	err = awaitBuildWithin(short2, silent, time.Second, 50*time.Millisecond, "new5678")
	if err == nil || !strings.Contains(err.Error(), "旧版本") {
		t.Fatalf("a machine still on the old program was not named: %v", err)
	}
}

// Which build a machine runs is read from the machine's own cluster service,
// so an upgrade can be confirmed from a node that does not coordinate.
func TestAskBuildReadsTheMemberStatusInsteadOfTheLocalApplication(t *testing.T) {
	nodes := testNodes(t, 1)
	nodes[0].config.Coordination.Build = "hub-build"
	r := openNode(t, nodes[0])
	ready(t, r)
	peer := &Peer{Config: PeerConfig{NodeID: "node-hub"}, client: nodes[0].client}
	peer.Runtime.Store(r)
	build, err := peer.askBuild("node-1")(t.Context())
	if err != nil || build != "hub-build" {
		t.Fatalf("member build = %q %v", build, err)
	}
	if _, err := peer.askBuild("node-absent")(t.Context()); err == nil || !strings.Contains(err.Error(), "成员") {
		t.Fatalf("a member outside the cluster was not refused: %v", err)
	}
}

// An upgrade asked for a node ID that is neither this node, a machine it
// keeps a link to, nor a cluster member is refused as unknown and leaves no
// record. Every machine the cluster knows keeps its usual answer: a member
// without a link is told it has no tunnel here. Without a running consensus
// runtime membership cannot be read, so no node ID is called unknown.
func TestUpgradeTellsAnUnknownNodeFromAMachineWithoutATunnel(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(config, []byte("# no hosts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	ready(t, r)
	peer := &Peer{Config: PeerConfig{NodeID: "node-hub", Links: map[string]PeerLink{"node-dev": {Alias: "dev"}}}}
	peer.Runtime.Store(r)
	service := sshconnect.New(sshconnect.Options{ConfigPath: config, Backend: peerSSHBackend{peer: peer}, InstallationMode: sshconnect.InstallPeer})
	t.Cleanup(func() { _ = service.Close() })
	code := func(node string) string {
		t.Helper()
		result, err := service.Upgrade(t.Context(), node)
		var step *sshconnect.StepError
		if !errors.As(err, &step) {
			t.Fatalf("%s: %#v %v", node, result, err)
		}
		if (step.Code == "unknown_node") != (result.Status == "") {
			t.Fatalf("%s: refused as %s with record %#v", node, step.Code, result)
		}
		return step.Code
	}
	for node, want := range map[string]string{"Mac mini": "unknown_node", "node-1": "upgrade_target", "node-hub": "upgrade_target", "node-dev": "unknown_alias"} {
		if got := code(node); got != want {
			t.Fatalf("%s refused as %s, want %s", node, got, want)
		}
	}
	var step *sshconnect.StepError
	if _, err := service.UpgradeStatus(t.Context(), "Mac mini"); !errors.As(err, &step) || step.Code != "unknown_node" {
		t.Fatalf("unknown node status = %v", err)
	}
	if status, err := service.UpgradeStatus(t.Context(), "node-1"); err != nil || status.Status != "needs_attention" {
		t.Fatalf("a member's refused upgrade is not readable: %#v %v", status, err)
	}
	peer.Runtime.Store(nil)
	if got := code("Mac mini"); got != "upgrade_target" {
		t.Fatalf("without a runtime an unlisted node was refused as %s", got)
	}
}
