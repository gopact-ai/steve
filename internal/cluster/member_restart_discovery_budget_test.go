package cluster

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/sshconnect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

type countedDiscoveryBackend struct {
	peerSSHBackend
	reads atomic.Int32
}

func (b *countedDiscoveryBackend) RestartTarget(ctx context.Context, node string) (string, error) {
	b.reads.Add(1)
	return b.peerSSHBackend.RestartTarget(ctx, node)
}

func TestRestartDiscoveryDoesNotStarveALaterHealthyHolder(t *testing.T) {
	hub := startTestHub(t)
	var candidates []*Peer
	for i := 0; i < 8; i++ {
		candidates = append(candidates, joinNonvoter(t, hub, nil))
	}
	target := joinNonvoter(t, hub, nil)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Config.NodeID < candidates[j].Config.NodeID })
	holder := candidates[len(candidates)-1]
	active := WaitPeerReady(t, hub)
	cfg := &config.Config{Gateway: config.Gateway{OwnerID: "test-owner", HomePath: "/fixture/home"}}
	if _, err := platformconfig.New(active.Ledger).Bootstrap(t.Context(), cfg, platformconfig.LocalNode{ID: hub.Config.NodeID, Config: config.Node{Addr: hub.Worker().Address, Token: hub.Worker().Token}}); err != nil {
		t.Fatal(err)
	}
	holder.Mu.Lock()
	holder.Config.Links = map[string]PeerLink{target.Config.NodeID: {Alias: "fixture-target"}}
	holder.Mu.Unlock()
	backend := &countedDiscoveryBackend{peerSSHBackend: peerSSHBackend{peer: holder}}
	holder.Mu.Lock()
	holder.localSSH = sshconnect.New(sshconnect.Options{Backend: backend, Runner: &restartWireRunner{}, InstallationMode: sshconnect.InstallPeer})
	holder.Mu.Unlock()
	control := hub.MemberRestarts(active)
	beforeCtx, beforeCancel := context.WithTimeout(t.Context(), 20*time.Second)
	got, err := control.Find(beforeCtx, target.Config.NodeID, "test-owner")
	beforeCancel()
	if err != nil || got.Holder != holder.Config.NodeID {
		t.Fatalf("healthy setup not discoverable: %+v %v", got, err)
	}
	// Block only discovery's holder-service access on the seven earlier peers.
	// Their control-plane runtimes remain alive; the final holder stays responsive.
	for _, peer := range candidates[:len(candidates)-1] {
		peer.Mu.Lock()
	}
	defer func() {
		for _, peer := range candidates[:len(candidates)-1] {
			peer.Mu.Unlock()
		}
	}()
	// Shorter budgets with the production ratio: a pass reaches about six
	// of the eight candidates, so only a second pass that goes on from the
	// first reaches the healthy holder ordered last. A probe of the healthy
	// holder has a whole second under the race detector.
	discovery := control.(memberRestarts).discovery
	discovery.passBudget, discovery.probeTimeout = 6500*time.Millisecond, time.Second
	if pass, probe := discovery.budgets(); pass/probe >= time.Duration(len(candidates)) {
		t.Fatalf("one pass of %s with %s probes could reach every candidate", pass, probe)
	}
	backend.reads.Store(0)
	for pass := 0; pass < 2; pass++ {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		got, err = control.Find(ctx, target.Config.NodeID, "test-owner")
		cancel()
		t.Logf("pass %d: holder=%q err=%v", pass, got.Holder, err)
		if err == nil {
			t.Fatal("unknown holders were treated as a unique link")
		}
		var incomplete MemberRestartError
		if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &incomplete) || incomplete.Reason != "restart_discovery_incomplete") {
			t.Fatalf("unexpected failure: %v", err)
		}
	}
	if backend.reads.Load() == 0 {
		t.Fatal("two complete reconciliation budgets never reached the healthy final holder")
	}
}
