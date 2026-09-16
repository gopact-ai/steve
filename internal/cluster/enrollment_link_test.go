package cluster

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/sshconnect"
	"github.com/hashicorp/raft"
)

// fakeTunnels stands in for the ssh client. Both ends of a test live on
// one machine, so every -L and -R forward becomes a local TCP proxy from
// its listen address to its target: exactly what the session would do.
type fakeTunnels struct {
	mu       sync.Mutex
	sessions []*fakeTunnel
}

type fakeTunnel struct {
	listeners []net.Listener
	done      chan struct{}
	once      sync.Once
}

func (f *fakeTunnels) Start(_ context.Context, args []string) (sshconnect.Process, error) {
	session := &fakeTunnel{done: make(chan struct{})}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-L" && args[i] != "-R" {
			continue
		}
		listen, target, ok := strings.Cut(args[i+1], ":127.0.0.1:")
		if !ok {
			return nil, errors.New("unexpected forward " + args[i+1])
		}
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			session.Kill()
			return nil, err
		}
		session.listeners = append(session.listeners, listener)
		go proxyTo(listener, "127.0.0.1:"+target)
	}
	f.mu.Lock()
	f.sessions = append(f.sessions, session)
	f.mu.Unlock()
	return session, nil
}

func proxyTo(listener net.Listener, target string) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			upstream, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				return
			}
			go func() { io.Copy(upstream, client); upstream.Close() }()
			io.Copy(client, upstream)
			client.Close()
		}()
	}
}

func (s *fakeTunnel) Wait() (string, error) {
	<-s.done
	return "", errors.New("killed")
}

func (s *fakeTunnel) Kill() {
	s.once.Do(func() {
		for _, l := range s.listeners {
			l.Close()
		}
		close(s.done)
	})
}

func (f *fakeTunnels) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

// A machine enrolled over SSH never has to reach this node on the network:
// the hub advertises an address nobody can route to, and the enrollment
// still completes because the two talk through the session's forwards.
// The link is recorded before the machine is installed, so a restart of
// the hub reopens it, and the machine's package tells it to reach the hub
// at the tunnel ports rather than at what the hub advertises.
func TestEnrollmentOverSSHCarriesTheClusterProtocolThroughTheSession(t *testing.T) {
	tunnels := &fakeTunnels{}
	hubOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	hubConfig, err := LoadClusterPeerConfig(hubOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	hubConfig.RaftAddress = "only-a-tunnel-reaches-it.invalid:0"
	hubConfig.PeerAddress = "only-a-tunnel-reaches-it.invalid:0"
	hubConfig.PeerURL = "https://only-a-tunnel-reaches-it.invalid:0"
	if err := SaveClusterJSON(hubOptions.ClusterPath, hubConfig, false); err != nil {
		t.Fatal(err)
	}
	var activations atomic.Int32
	hubOptions.Activate = testPeerApplication(t, &activations)
	hubOptions.SSHLaunch = tunnels
	hub := StartTestPeer(t, hubOptions)
	WaitPeerReady(t, hub)

	peerAddress, raftAddress := FreeEnrollmentPorts(t)
	hubRaftOnMachine, hubAPIOnMachine := FreeEnrollmentPorts(t)
	hubRoute := coordination.Route{Raft: hubRaftOnMachine, API: hubAPIOnMachine}
	request := PeerEnrollmentRequest{Alias: "box", Name: "box", PeerAddress: peerAddress, RaftAddress: raftAddress, HubRoute: hubRoute, Level: "restricted"}
	plan, err := hub.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatalf("a routed enrollment was refused: %v", err)
	}
	if !strings.Contains(strings.Join(plan.Effects, "\n"), hubRaftOnMachine) {
		t.Fatalf("the plan does not tell the user about the tunnel: %v", plan.Effects)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	const id = "over-ssh"
	prepared, err := hub.PrepareEnrollment(t.Context(), request, id, true)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ImportPeerPackage(prepared.Payload, ClusterPeerTestDir(t)+"/box")
	if err != nil {
		t.Fatal(err)
	}
	nodeConfig, err := LoadClusterPeerConfig(imported.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if nodeConfig.Routes[hub.Config.NodeID] != hubRoute {
		t.Fatalf("the machine was not told to reach the hub through the tunnel: %+v", nodeConfig.Routes)
	}

	linkCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := hub.OpenEnrollmentLink(linkCtx, id); err != nil {
		t.Fatalf("the link did not come up: %v", err)
	}
	route, ok := hub.routes.Lookup(prepared.NodeID)
	if !ok || !strings.HasPrefix(route.Raft, "127.0.0.1:") || !strings.HasPrefix(route.API, "127.0.0.1:") {
		t.Fatalf("the hub has no route to the machine through the link: %+v %v", route, ok)
	}
	saved, err := LoadClusterPeerConfig(hubOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if link := saved.Links[prepared.NodeID]; link.Alias != "box" || link.Remote != hubRoute || !strings.HasSuffix(link.Peer.API, ":"+peerAddress[strings.LastIndex(peerAddress, ":")+1:]) {
		t.Fatalf("the link was not recorded for the next start: %+v", saved.Links)
	}
	// The same enrollment asking again keeps the session it has.
	if err := hub.OpenEnrollmentLink(linkCtx, id); err != nil || tunnels.count() != 1 {
		t.Fatalf("a second request replaced a working link: %v (%d sessions)", err, tunnels.count())
	}

	nodeOptions := PeerOptions{ConfigPath: imported.ConfigPath, ClusterPath: imported.ClusterPath, RaftConfig: raft.DefaultConfig(), PollInterval: 25 * time.Millisecond, TestFailureDomain: func() (string, error) { return "test-domain-box", nil }, Activate: testPeerApplication(t, &activations)}
	node := StartTestPeer(t, nodeOptions)
	// The enrollment goes from waiting for the node, through the join and
	// its mesh check, to registering the worker; that last step needs the
	// real application, which this fixture does not run. Everything before
	// it is the protocol crossing the tunnel in both directions.
	deadline := time.Now().Add(30 * time.Second)
	var result PeerEnrollmentResult
	for {
		result, err = hub.CompletePeerEnrollment(t.Context(), id)
		if result.Phase == "registering_worker" || err == nil && result.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node did not join through the tunnel: phase=%s error=%v", result.Phase, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	state, err := hub.Runtime.Load().ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Voters[node.Config.NodeID] == "" || result.NodeID != node.Config.NodeID {
		t.Fatalf("the machine is not a voter after joining through the tunnel: %+v", state.Voters)
	}
	if _, ok := hub.LinkStatuses()[node.Config.NodeID]; !ok {
		t.Fatal("the link to a joined machine was dropped")
	}
}

// Giving up an enrollment closes its session and forgets the link and the
// route, so nothing keeps dialing a machine that is not coming.
func TestAbandoningAnEnrollmentDropsItsLink(t *testing.T) {
	tunnels := &fakeTunnels{}
	hubOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var activations atomic.Int32
	hubOptions.Activate = testPeerApplication(t, &activations)
	hubOptions.SSHLaunch = tunnels
	hub := StartTestPeer(t, hubOptions)
	WaitPeerReady(t, hub)
	peerAddress, raftAddress := FreeEnrollmentPorts(t)
	hubRaftOnMachine, hubAPIOnMachine := FreeEnrollmentPorts(t)
	request := PeerEnrollmentRequest{Alias: "box", Name: "box", PeerAddress: peerAddress, RaftAddress: raftAddress, HubRoute: coordination.Route{Raft: hubRaftOnMachine, API: hubAPIOnMachine}, Level: "restricted"}
	plan, err := hub.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	prepared, err := hub.PrepareEnrollment(t.Context(), request, "given-up", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := hub.OpenEnrollmentLink(ctx, "given-up"); err != nil {
		t.Fatal(err)
	}
	if err := hub.AbandonPeerEnrollment(t.Context(), "given-up"); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.routes.Lookup(prepared.NodeID); ok {
		t.Fatal("the route to an abandoned machine remains")
	}
	if _, ok := hub.LinkStatuses()[prepared.NodeID]; ok {
		t.Fatal("the link to an abandoned machine remains")
	}
	saved, err := LoadClusterPeerConfig(hubOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Links[prepared.NodeID]; ok {
		t.Fatal("the abandoned link would be reopened at the next start")
	}
	select {
	case <-tunnels.sessions[0].done:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned session was not ended")
	}
}
