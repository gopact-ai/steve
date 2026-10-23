package cluster

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

func discoveryHolders(t *testing.T) (MemberRestarts, string, []*Peer) {
	t.Helper()
	hub := startTestHub(t)
	holders := []*Peer{joinNonvoter(t, hub, nil), joinNonvoter(t, hub, nil)}
	target := joinNonvoter(t, hub, nil)
	active := WaitPeerReady(t, hub)
	cfg := &config.Config{Gateway: config.Gateway{OwnerID: "test-owner", HomePath: "/fixture/home"}}
	if _, err := platformconfig.New(active.Ledger).Bootstrap(t.Context(), cfg, platformconfig.LocalNode{ID: hub.Config.NodeID, Config: config.Node{Addr: hub.Worker().Address, Token: hub.Worker().Token}}); err != nil {
		t.Fatal(err)
	}
	for _, holder := range holders {
		holder.Mu.Lock()
		holder.Config.Links = map[string]PeerLink{target.Config.NodeID: {Alias: "fixture-target"}}
		holder.Mu.Unlock()
	}
	return hub.MemberRestarts(active), target.Config.NodeID, holders
}

func TestMemberRestartDiscoveryRefreshesLinkOwnershipAcrossCalls(t *testing.T) {
	control, target, holders := discoveryHolders(t)
	copy := control
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := control.Find(ctx, target, "test-owner"); err == nil {
		t.Fatal("multiple idle holders were called unique")
	}
	holders[0].Mu.Lock()
	holders[0].Config.Links = nil
	holders[0].Mu.Unlock()
	chosen, err := copy.Find(ctx, target, "test-owner")
	if err != nil || chosen.Holder != holders[1].Config.NodeID {
		t.Fatalf("current unique holder not found: %+v %v", chosen, err)
	}
	holders[1].Mu.Lock()
	holders[1].Config.Links = nil
	holders[1].Mu.Unlock()
	holders[0].Mu.Lock()
	holders[0].Config.Links = map[string]PeerLink{target: {Alias: "fixture-target"}}
	holders[0].Mu.Unlock()
	chosen, err = control.Find(ctx, target, "test-owner")
	if err != nil || chosen.Holder != holders[0].Config.NodeID {
		t.Fatalf("stale holder selection was reused: %+v %v", chosen, err)
	}
}

func TestConcurrentMemberRestartDiscoverySharesProbeReservations(t *testing.T) {
	control, _, _ := discoveryHolders(t)
	one := control.(memberRestarts)
	two := one
	ready := make(chan struct{})
	results := make(chan string, 2)
	var calls sync.WaitGroup
	for _, client := range []memberRestarts{one, two} {
		calls.Add(1)
		go func(m memberRestarts) {
			defer calls.Done()
			<-ready
			results <- m.discovery.next([]string{"first", "second"}, map[string]bool{})
		}(client)
	}
	close(ready)
	calls.Wait()
	first, second := <-results, <-results
	if first == second || first == "" || second == "" {
		t.Fatalf("copies did not share probe reservations: %q %q", first, second)
	}
}
