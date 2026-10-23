package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/sshconnect"
	"github.com/gopact-ai/steve/internal/sshconnect/linktest"
)

// A member this node keeps a link to is restarted over that link's alias;
// this node itself, and a member it has no link to, are not restarted
// from here. Each refusal is what the page shows by a restart it cannot
// offer, so it says where to turn instead: a member without a link here
// is restarted, and its automatic start seen, where it joined over SSH.
func TestRestartTargetNamesTheLinkAliasAndRefusesSelf(t *testing.T) {
	peer := &Peer{Config: PeerConfig{NodeID: "node-hub", Links: map[string]PeerLink{"node-dev": {Alias: "dev"}}}}
	backend := peerSSHBackend{peer: peer}
	if alias, err := backend.RestartTarget(t.Context(), "node-dev"); err != nil || alias != "dev" {
		t.Fatalf("target = %q %v", alias, err)
	}
	if _, err := backend.RestartTarget(t.Context(), "node-hub"); err == nil || err.Error() != "不能从这里重启当前提供控制台的节点" {
		t.Fatalf("self was not refused: %v", err)
	}
	_, err := backend.RestartTarget(t.Context(), "node-other")
	if err == nil || !strings.Contains(err.Error(), "本节点没有这台机器的 SSH 连接") || !strings.Contains(err.Error(), "把它加入集群的节点的控制台") || !strings.Contains(err.Error(), "自动拉起") {
		t.Fatalf("a member without a link was not refused with where to turn: %v", err)
	}
	english := i18n.WithLocale(t.Context(), i18n.LocaleEN)
	if _, err := backend.RestartTarget(english, "node-other"); err == nil || !strings.Contains(err.Error(), "console of the node that added it") {
		t.Fatalf("a member without a link was not refused in English: %v", err)
	}
}

// After a restart the machine only has to answer again: whichever build it
// reports is accepted, an ask that hangs is given up on so the next one
// reaches the new process, and running out of time says it never answered.
func TestAwaitAnswerAcceptsWhicheverBuildTheMachineRuns(t *testing.T) {
	asks := 0
	ask := func(ctx context.Context) (string, error) {
		asks++
		switch asks {
		case 1:
			return "", errors.New("connection reset")
		case 2:
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "old1234", nil
	}
	var reported []string
	ctx := sshconnect.WithReporter(t.Context(), func(text string) { reported = append(reported, text) })
	if err := awaitAnswerWithin(ctx, ask, 3*time.Second, 50*time.Millisecond); err != nil {
		t.Fatalf("an answer on any build was not accepted: %v (asks=%d)", err, asks)
	}
	if asks != 3 || len(reported) != 2 || !strings.Contains(reported[0], "connection reset") || !strings.Contains(reported[1], "重新询问") {
		t.Fatalf("asks=%d reported=%v", asks, reported)
	}
	silent := func(context.Context) (string, error) { return "", nil }
	if err := awaitAnswerWithin(t.Context(), silent, time.Second, 50*time.Millisecond); err != nil {
		t.Fatalf("a machine answering without a build was not accepted: %v", err)
	}
	down := func(context.Context) (string, error) { return "", errors.New("connection refused") }
	short, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := awaitAnswerWithin(short, down, time.Second, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "还没有重新应答") {
		t.Fatalf("running out of time did not say the machine never answered: %v", err)
	}
}

// A restarted machine is back once it answers the cluster itself; one that
// is not a member never is, and the wait ends when its time runs out.
func TestRestartedWaitsForTheMachineToAnswerTheCluster(t *testing.T) {
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	ready(t, r)
	peer := &Peer{Config: PeerConfig{NodeID: "node-hub"}, client: nodes[0].client, ctx: t.Context()}
	peer.Runtime.Store(r)
	backend := peerSSHBackend{peer: peer}
	var reported []string
	ctx := sshconnect.WithReporter(t.Context(), func(text string) { reported = append(reported, text) })
	if err := backend.Restarted(ctx, "node-1"); err != nil {
		t.Fatalf("an answering member was not taken as back: %v", err)
	}
	if len(reported) == 0 || !strings.Contains(reported[0], "等待机器回到集群") {
		t.Fatalf("the wait did not say what it waits for: %v", reported)
	}
	short, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := backend.Restarted(short, "node-absent"); err == nil || !strings.Contains(err.Error(), "还没有重新应答") {
		t.Fatalf("a machine outside the cluster was taken as back: %v", err)
	}
}

// A restart or an upgrade waits for its machine past the page that asked
// for it, but not past this node: closing the node ends the wait at once.
func TestWaitingForARestartedOrUpgradedMachineEndsWhenThisNodeCloses(t *testing.T) {
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	ready(t, r)
	for _, wait := range []struct {
		name string
		wait func(peerSSHBackend, context.Context) error
	}{
		{"restart", func(b peerSSHBackend, ctx context.Context) error { return b.Restarted(ctx, "node-2") }},
		{"upgrade", func(b peerSSHBackend, ctx context.Context) error { return b.Upgraded(ctx, "node-2") }},
	} {
		t.Run(wait.name, func(t *testing.T) {
			// The machine is not a member (the one node running is node-1)
			// and its session never opens, so neither wait ends on its own.
			peerCtx, closePeer := context.WithCancel(t.Context())
			defer closePeer()
			launcher := &linktest.Launcher{Refuse: errors.New("ssh: connect to host dev port 22: Connection refused")}
			peer := &Peer{Options: PeerOptions{SSHLaunch: launcher}, Config: PeerConfig{NodeID: "node-hub", Links: map[string]PeerLink{"node-2": {Alias: "dev"}}}, client: nodes[0].client, routes: coordination.NewRouteTable(nil), ctx: peerCtx, linkCtx: t.Context()}
			peer.Runtime.Store(r)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- wait.wait(peerSSHBackend{peer: peer}, ctx) }()
			closePeer()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a machine that never came back was taken as back")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("closing this node did not end the wait for the machine")
			}
		})
	}
}

// Automatic start watches the machines this node keeps a link to that are
// still members, and none before the node's runtime is up; a machine
// answers when its own cluster service does.
func TestAutoStartWatchesLinkedMembersThatStillBelong(t *testing.T) {
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	ready(t, r)
	peer := &Peer{Config: PeerConfig{NodeID: "node-hub", Links: map[string]PeerLink{"node-1": {Alias: "dev"}, "node-gone": {Alias: "gone"}}}, client: nodes[0].client}
	backend := peerSSHBackend{peer: peer}
	if watched := backend.Watched(t.Context()); len(watched) != 0 {
		t.Fatalf("machines were watched before the runtime was up: %v", watched)
	}
	peer.Runtime.Store(r)
	if watched := backend.Watched(t.Context()); !slices.Equal(watched, []string{"node-1"}) {
		t.Fatalf("watched = %v, want only the linked member", watched)
	}
	if !backend.Answers(t.Context(), "node-1") {
		t.Fatal("an answering member was taken as down")
	}
	if backend.Answers(t.Context(), "node-gone") {
		t.Fatal("a machine outside the cluster was taken as answering")
	}
}

// Of the machines with a link here, automatic start watches the members
// other than this node, and stops watching one as soon as its removal
// begins, before it has left the cluster.
func TestAutoStartDoesNotWatchAMemberBeingRemoved(t *testing.T) {
	state := coordination.State{
		Members:  map[string]coordination.Member{"node-hub": {NodeID: "node-hub"}, "node-1": {NodeID: "node-1"}, "node-2": {NodeID: "node-2"}, "node-3": {NodeID: "node-3"}},
		Removing: map[string]bool{"node-2": true},
	}
	links := map[string]PeerLink{"node-hub": {Alias: "hub"}, "node-1": {Alias: "dev"}, "node-2": {Alias: "dev2"}, "node-gone": {Alias: "gone"}}
	if watched := watchedMembers(state, links, "node-hub"); !slices.Equal(watched, []string{"node-1"}) {
		t.Fatalf("watched = %v, want only the linked member that stays", watched)
	}
}

// A machine is reachable while the SSH session this node keeps to it is up;
// one whose session cannot start, or that has none, is not.
func TestAutoStartReachesAMachineOnlyThroughItsLiveSession(t *testing.T) {
	up := &linktest.Launcher{}
	down := &linktest.Launcher{Refuse: errors.New("ssh: connect to host dev port 22: Connection refused")}
	spec := func(alias string, launcher *linktest.Launcher) *sshconnect.Link {
		link := sshconnect.OpenLink(t.Context(), sshconnect.LinkSpec{Alias: alias, Inbound: []sshconnect.PortForward{{Listen: up.Reserve(t), Target: "127.0.0.1:1"}}}, sshconnect.LinkOptions{Launch: launcher, Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
		t.Cleanup(link.Close)
		return link
	}
	live := spec("dev", up)
	peer := &Peer{Config: PeerConfig{NodeID: "node-hub"}, links: map[string]*sshconnect.Link{"node-dev": live, "node-down": spec("down", down)}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := live.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	backend := peerSSHBackend{peer: peer}
	if !backend.Reachable(t.Context(), "node-dev") {
		t.Fatal("a machine with a live session was taken as unreachable")
	}
	for _, nodeID := range []string{"node-down", "node-none"} {
		if backend.Reachable(t.Context(), nodeID) {
			t.Fatalf("%s was taken as reachable", nodeID)
		}
	}
}

// A restart is kept among the coordinator's events as done by the node
// that asked to record it, whatever that node claims; a request that does
// not come from a cluster member is refused.
func TestARestartIsRecordedByTheCoordinatorAsDoneByTheNodeThatRanIt(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	admin := &adminsvc.Service{View: readmodel.New(readmodel.Sources{})}
	var activations atomic.Int32
	application := testPeerApplication(t, &activations)
	options.Activate = func(ctx context.Context, activation Activation, ready func(PeerApplicationEndpoint) error) (Deactivate, error) {
		return application(ctx, activation, func(endpoint PeerApplicationEndpoint) error {
			endpoint.Admin = admin
			return ready(endpoint)
		})
	}
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	at := time.Date(2026, 9, 29, 8, 30, 0, 0, time.UTC)
	record := sshconnect.RestartRecord{NodeID: "node-dev", By: "node-forged", Automatic: true, Outcome: sshconnect.RestartStarted, At: at}
	if err := (peerSSHBackend{peer: peer}).RecordRestart(t.Context(), record); err != nil {
		t.Fatalf("the restart was not recorded: %v", err)
	}
	history, _, err := admin.View.History(t.Context(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var found *readmodel.HistoryEntry
	for i := range history {
		if history[i].Kind == "observe.node.restart" {
			found = &history[i]
		}
	}
	if found == nil || found.Subject != "node-dev" || !found.At.Equal(at) || found.Data["by"] != peer.Config.NodeID || found.Data["trigger"] != "automatic" || found.Data["outcome"] != sshconnect.RestartStarted {
		t.Fatalf("recorded restart = %#v in %#v", found, history)
	}
	response := httptest.NewRecorder()
	peer.serveNodeRestart(response, httptest.NewRequest(http.MethodPost, "/cluster/node-restart", strings.NewReader(`{"node_id":"node-dev","outcome":"restarted"}`)))
	if response.Code != http.StatusForbidden {
		t.Fatalf("a request from outside the cluster = %d %s", response.Code, response.Body)
	}
}

// A node without a runtime has no coordinator to record a restart with,
// and says so instead of dropping it.
func TestARestartRecordWithoutARuntimeIsRefused(t *testing.T) {
	peer := &Peer{Config: PeerConfig{NodeID: "node-hub"}}
	err := (peerSSHBackend{peer: peer}).RecordRestart(t.Context(), sshconnect.RestartRecord{NodeID: "node-dev", Outcome: sshconnect.RestartRestarted})
	if err == nil || !strings.Contains(err.Error(), "集群服务没有运行") {
		t.Fatalf("recording without a runtime = %v", err)
	}
}

// A node opened to start peers watches the machines it keeps links to from
// the moment it runs, without waiting for its console to be opened; one
// opened without it starts no peer on its own.
func TestAPeerOpenedToStartPeersWatchesItsMachinesFromTheStart(t *testing.T) {
	for _, starts := range []bool{false, true} {
		options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
		var activations atomic.Int32
		options.Activate = testPeerApplication(t, &activations)
		options.AutoStartPeers = starts
		peer := StartTestPeer(t, options)
		peer.Mu.RLock()
		watching := peer.localSSH != nil
		peer.Mu.RUnlock()
		if watching != starts {
			t.Fatalf("with AutoStartPeers %v the node watches its machines: %v", starts, watching)
		}
		if err := peer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
