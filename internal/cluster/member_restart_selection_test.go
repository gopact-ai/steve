package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

func TestRestartSelectionRefusesAmbiguousLinksAndPlans(t *testing.T) {
	idle := func(holder string) restartCandidate {
		return restartCandidate{holder: holder, memberRestartResponse: memberRestartResponse{Restartable: true}}
	}
	active := func(holder, plan, kind string) restartCandidate {
		candidate := idle(holder)
		candidate.Operation = sshconnect.MemberRestart{PlanID: plan, Kind: kind, State: "running"}
		return candidate
	}
	for _, tc := range []struct {
		name       string
		candidates []restartCandidate
		reason     string
		holder     string
		plan       string
	}{
		{"multiple idle", []restartCandidate{idle("one"), idle("two")}, "restart_ambiguous_holder", "", ""},
		{"multiple active", []restartCandidate{active("one", "restart-one", "restart"), active("two", "upgrade-two", "upgrade")}, "restart_conflicting_plans", "", ""},
		{"join active behind idle", []restartCandidate{idle("one"), active("two", "upgrade-two", "upgrade")}, "", "two", "upgrade-two"},
		{"one idle", []restartCandidate{idle("one")}, "", "one", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chooseRestartTarget("cluster", 11, tc.candidates)
			var refused MemberRestartError
			if tc.reason != "" {
				if !errors.As(err, &refused) || refused.Reason != tc.reason {
					t.Fatalf("ambiguous selection accepted: %+v %v", got, err)
				}
				return
			}
			if err != nil || got.Holder != tc.holder || got.Selection.JoinPlanID != tc.plan || got.Selection.MembershipRevision != 11 {
				t.Fatalf("selection differs from observed plan: %+v %v", got, err)
			}
		})
	}
}

func TestRestartIncompleteCandidateViewDoesNotProveUniqueOwnership(t *testing.T) {
	control, target, holders := discoveryHolders(t)
	holders[1].Mu.Lock()
	holders[1].Config.Links = nil
	holders[1].Mu.Unlock()
	if _, err := control.Find(t.Context(), target, "test-owner"); err != nil {
		t.Fatal(err)
	}
	// Losing this answer means its present ownership/activity is unknown,
	// not that it ceased to be a candidate.
	holders[1].Mu.Lock()
	defer holders[1].Mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := control.Find(ctx, target, "test-owner")
	var incomplete MemberRestartError
	if !errors.As(err, &incomplete) || incomplete.Reason != "restart_discovery_incomplete" || got.Holder != "" {
		t.Fatalf("unknown member was silently excluded: %+v %v", got, err)
	}
}

func TestRestartMembershipChangeInvalidatesTheSelection(t *testing.T) {
	control, target, holders := discoveryHolders(t)
	holders[1].Mu.Lock()
	holders[1].Config.Links = nil
	holders[1].Mu.Unlock()
	got, err := control.Find(t.Context(), target, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	member := control.(memberRestarts)
	joinNonvoter(t, member.peer, nil)
	op := attempt.ForceRestart{ID: "old-membership", ClusterID: got.ClusterID, NodeID: target, Holder: got.Holder, By: "test-owner", RequestedAt: time.Now(), Selection: got.Selection}
	if err := control.Start(t.Context(), op); err == nil {
		t.Fatal("old membership view authorized dispatch")
	}
}

func TestRestartDiscoveryDoesNotQueryItsUnresponsiveTarget(t *testing.T) {
	hub := startTestHub(t)
	target := joinNonvoter(t, hub, nil)
	active := WaitPeerReady(t, hub)
	cfg := &config.Config{Gateway: config.Gateway{OwnerID: "test-owner", HomePath: "/fixture/home"}}
	if _, err := platformconfig.New(active.Ledger).Bootstrap(t.Context(), cfg, platformconfig.LocalNode{ID: hub.Config.NodeID, Config: config.Node{Addr: hub.Worker().Address, Token: hub.Worker().Token}}); err != nil {
		t.Fatal(err)
	}
	hub.Mu.Lock()
	hub.Config.Links = map[string]PeerLink{target.Config.NodeID: {Alias: "fixture-target"}}
	hub.Mu.Unlock()
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	chosen, err := hub.MemberRestarts(active).Find(ctx, target.Config.NodeID, "test-owner")
	if err != nil || chosen.Holder != hub.Config.NodeID || chosen.Selection.JoinPlanID != "" {
		t.Fatalf("the unreachable target was treated as its own potential holder: %+v %v", chosen, err)
	}
}

func TestRestartDiscoveryKeepsRemovingCandidatesInItsView(t *testing.T) {
	for _, mode := range []string{"idle-no-link", "idle-with-link", "unanswered"} {
		t.Run(mode, func(t *testing.T) {
			control, target, holders := discoveryHolders(t)
			m := control.(memberRestarts)
			if mode != "idle-with-link" {
				holders[1].Mu.Lock()
				holders[1].Config.Links = nil
				holders[1].Mu.Unlock()
			}
			state, authority, err := m.authority(t.Context(), target, "test-owner")
			if err != nil {
				t.Fatal(err)
			}
			state.Removing = map[string]bool{holders[1].Config.NodeID: true}
			if mode == "unanswered" {
				holders[1].Mu.Lock()
				defer holders[1].Mu.Unlock()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			candidates, err := m.discover(ctx, target, "test-owner", state, authority)
			if mode == "idle-no-link" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := chooseRestartTarget(m.peer.Config.ClusterID, state.Revision, candidates)
				if err != nil || got.Holder != holders[0].Config.NodeID {
					t.Fatalf("a proved idle non-holder blocked the unique eligible holder: %+v %v", got, err)
				}
			} else {
				var refused MemberRestartError
				want := "restart_discovery_changed"
				if mode == "unanswered" {
					want = "restart_discovery_incomplete"
				}
				if !errors.As(err, &refused) || refused.Reason != want {
					t.Fatalf("removing member disappeared from ownership proof: candidates=%+v err=%v", candidates, err)
				}
			}
		})
	}
}
