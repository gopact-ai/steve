package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/nodewire"
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

// The wait keeps asking until the machine answers with the wanted build:
// an ask that hangs is given up on so the next one can reach the restarted
// process, a few seconds without a coordinator (the application rebuilds
// when the restarted member led the cluster) are waited out, and running
// out of time names the version last seen.
func TestAwaitBuildAsksAgainUntilTheMachineReportsTheBuild(t *testing.T) {
	asks, turns := 0, 0
	refresh := func(ctx context.Context, nodeID string) (nodewire.Advert, error) {
		asks++
		switch asks {
		case 1:
			return nodewire.Advert{}, errors.New("connection reset")
		case 2:
			<-ctx.Done()
			return nodewire.Advert{}, ctx.Err()
		case 3:
			return nodewire.Advert{BuildVersion: "old1234"}, nil
		}
		return nodewire.Advert{BuildVersion: "new5678"}, nil
	}
	refresher := func() advertRefresh {
		turns++
		if turns == 4 || turns == 5 {
			return nil
		}
		return refresh
	}
	var reported []string
	ctx := sshconnect.WithReporter(t.Context(), func(text string) { reported = append(reported, text) })
	if err := awaitBuildWithin(ctx, refresher, 3*time.Second, 50*time.Millisecond, "node-dev", "new5678"); err != nil {
		t.Fatalf("the machine's new build was not accepted: %v (asks=%d)", err, asks)
	}
	if asks != 4 || turns != 6 || len(reported) != 3 || !strings.Contains(reported[0], "connection reset") || !strings.Contains(reported[1], "重新询问") || !strings.Contains(reported[2], "协调服务正在重启") {
		t.Fatalf("asks=%d turns=%d reported=%v", asks, turns, reported)
	}
	stale := func() advertRefresh {
		return func(context.Context, string) (nodewire.Advert, error) {
			return nodewire.Advert{BuildVersion: "old1234"}, nil
		}
	}
	short, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	err := awaitBuildWithin(short, stale, time.Second, 50*time.Millisecond, "node-dev", "new5678")
	if err == nil || !strings.Contains(err.Error(), "old1234") {
		t.Fatalf("running out of time did not name the version seen: %v", err)
	}
	// A coordinator that never comes back is this node's problem, not the
	// machine's, and saying so is what tells the two apart.
	none := func() advertRefresh { return nil }
	short2, cancel2 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel2()
	err = awaitBuildWithin(short2, none, time.Second, 50*time.Millisecond, "node-dev", "new5678")
	if err == nil || !strings.Contains(err.Error(), "协调服务") {
		t.Fatalf("a coordinator that never returned was blamed on the machine: %v", err)
	}
}
