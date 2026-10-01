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

func TestMemberRestartDiscoverySharesProgressAcrossCalls(t *testing.T) {
	control, target, _ := discoveryHolders(t)
	copy := control
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := control.Find(ctx, target, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	second, err := copy.Find(ctx, target, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	if first.Holder == second.Holder {
		t.Fatalf("successive discoveries restarted at the same member: %s", first.Holder)
	}
	third, err := control.Find(ctx, target, "test-owner")
	if err != nil || third.Holder != first.Holder {
		t.Fatalf("discovery did not wrap fairly: first=%+v third=%+v err=%v", first, third, err)
	}
}

func TestConcurrentMemberRestartDiscoverySharesProbeReservations(t *testing.T) {
	control, target, _ := discoveryHolders(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{})
	results := make(chan RestartTarget, 2)
	failures := make(chan error, 2)
	var calls sync.WaitGroup
	for range 2 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			<-ready
			got, err := control.Find(ctx, target, "test-owner")
			results <- got
			failures <- err
		}()
	}
	close(ready)
	calls.Wait()
	one, two := <-results, <-results
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	if one.Holder == two.Holder || one.Holder == "" || two.Holder == "" {
		t.Fatalf("concurrent discoveries did not share progress: %+v %+v", one, two)
	}
}
