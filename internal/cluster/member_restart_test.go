package cluster

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/nodewire"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

func TestMemberRestartDiscoveryPrefersTheCoordinatorLink(t *testing.T) {
	hub := startTestHub(t)
	member := joinNonvoter(t, hub, nil)
	active := WaitPeerReady(t, hub)
	cfg := &config.Config{Gateway: config.Gateway{OwnerID: "test-owner", HomePath: "/fixture/home"}}
	if _, err := platformconfig.New(active.Ledger).Bootstrap(t.Context(), cfg, platformconfig.LocalNode{ID: hub.Config.NodeID, Config: config.Node{Addr: hub.Worker().Address, Token: hub.Worker().Token}}); err != nil {
		t.Fatal(err)
	}
	hub.Mu.Lock()
	hub.Config.Links = map[string]PeerLink{member.Config.NodeID: {Alias: "fixture-link"}}
	hub.Mu.Unlock()
	control := hub.MemberRestarts(active)
	target, err := control.Find(t.Context(), member.Config.NodeID, "test-owner")
	if err != nil || target.ClusterID != hub.Config.ClusterID || target.Holder != hub.Config.NodeID {
		t.Fatalf("local holder not selected: %+v %v", target, err)
	}
	if _, err := control.Find(t.Context(), hub.Config.NodeID, "test-owner"); err == nil {
		t.Fatal("coordinator may restart itself")
	}
	if _, err := control.Find(t.Context(), member.Config.NodeID, "other-owner"); err == nil {
		t.Fatal("non-owner may restart a member")
	}
	ctx, cancel := context.WithCancel(active.Context)
	cancel()
	old := active
	old.Context = ctx
	if _, err := hub.MemberRestarts(old).Find(t.Context(), member.Config.NodeID, "test-owner"); err == nil {
		t.Fatal("inactive coordinator may restart a member")
	}
}

func TestMemberRestartRouteRefusesARegularMember(t *testing.T) {
	hub := startTestHub(t)
	member := joinNonvoter(t, hub, nil)
	active := WaitPeerReady(t, hub)
	target := hub.Runtime.Load().Status().Members[hub.Config.NodeID]
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var result map[string]any
	err := member.peerJSON(ctx, target, http.MethodPost, "/cluster/member-restart", map[string]any{"node_id": member.Config.NodeID, "epoch": active.Assignment.Epoch}, &result)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("non-coordinator response: %v", err)
	}
}

func TestMemberRestartWithoutPeerIdentityIsForbidden(t *testing.T) {
	p := &Peer{}
	response := httptest.NewRecorder()
	p.serveMemberRestart(response, httptest.NewRequest(http.MethodPost, "/cluster/member-restart", strings.NewReader(`{}`)))
	if response.Code != http.StatusForbidden {
		t.Fatalf("untrusted restart: %d", response.Code)
	}
}

func TestMemberRestartDiscoveryUsesAnotherLinkHolder(t *testing.T) {
	hub := startTestHub(t)
	holder := joinNonvoter(t, hub, nil)
	target := joinNonvoter(t, hub, nil)
	active := WaitPeerReady(t, hub)
	cfg := &config.Config{Gateway: config.Gateway{OwnerID: "test-owner", HomePath: "/fixture/home"}}
	if _, err := platformconfig.New(active.Ledger).Bootstrap(t.Context(), cfg, platformconfig.LocalNode{ID: hub.Config.NodeID, Config: config.Node{Addr: hub.Worker().Address, Token: hub.Worker().Token}}); err != nil {
		t.Fatal(err)
	}
	holder.Mu.Lock()
	holder.Config.Links = map[string]PeerLink{target.Config.NodeID: {Alias: "private-fixture-link"}}
	holder.Mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := hub.MemberRestarts(active).Find(ctx, target.Config.NodeID, "test-owner")
	if err != nil || got.Holder != holder.Config.NodeID {
		t.Fatalf("remote link holder not found: %+v %v", got, err)
	}
	holder.Mu.Lock()
	holder.Config.Links = nil
	holder.Mu.Unlock()
	_, err = hub.MemberRestarts(active).Find(ctx, target.Config.NodeID, "test-owner")
	var unavailable MemberRestartError
	if !errors.As(err, &unavailable) || unavailable.Reason != "restart_no_holder" {
		t.Fatalf("missing holder: %v", err)
	}
}

func TestMemberRestartClaimsFenceAuthorityAndOwnerChanges(t *testing.T) {
	hub := startTestHub(t)
	holder := joinNonvoter(t, hub, nil)
	target := joinNonvoter(t, hub, nil)
	active := WaitPeerReady(t, hub)
	cfg := &config.Config{Gateway: config.Gateway{OwnerID: "test-owner", HomePath: "/fixture/home"}}
	if _, err := platformconfig.New(active.Ledger).Bootstrap(t.Context(), cfg, platformconfig.LocalNode{ID: hub.Config.NodeID, Config: config.Node{Addr: hub.Worker().Address, Token: hub.Worker().Token}}); err != nil {
		t.Fatal(err)
	}
	op := attempt.ForceRestart{ID: "op", ClusterID: hub.Config.ClusterID, NodeID: target.Config.NodeID, Holder: holder.Config.NodeID, By: "test-owner", RequestedAt: time.Now().UTC()}
	if err := active.Ledger.PutBinding(t.Context(), "force-stop-member-restart", op.NodeID, op); err != nil {
		t.Fatal(err)
	}
	authority := nodewire.SessionAuthority{ClusterID: hub.Config.ClusterID, CoordinatorNodeID: hub.Config.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration}
	req := memberRestartRequest{Operation: op, Authority: authority, PlanID: "plan", Kind: "restart"}
	yes, err := holder.claimMemberRestart(t.Context(), req)
	if err != nil || !yes {
		t.Fatalf("valid holder claim: %v %v", yes, err)
	}
	yes, err = holder.claimMemberRestart(t.Context(), req)
	if err != nil || yes {
		t.Fatalf("claim repeated after lost answer: %v %v", yes, err)
	}
	req.Authority.CoordinatorEpoch++
	if _, err := hub.restartAuthority(t.Context(), req.Authority, op.NodeID); err == nil {
		t.Fatal("stale authority passed the dispatch gate")
	}
	if yes, err := holder.claimMemberRestart(t.Context(), req); yes || err == nil {
		t.Fatal("stale coordinator claim accepted")
	}
	req.Authority = authority
	if yes, err := target.claimMemberRestart(t.Context(), req); yes || err == nil {
		t.Fatal("wrong holder claimed")
	}
	declaration, _, err := platformconfig.New(active.Ledger).Load()
	if err != nil {
		t.Fatal(err)
	}
	declaration.Settings.Gateway.OwnerID = "new-owner"
	declaration.Channels.Console.OwnerID = "new-owner"
	if _, err := platformconfig.New(active.Ledger).Save(t.Context(), declaration.Revision, declaration); err != nil {
		t.Fatal(err)
	}
	if yes, err := holder.claimMemberRestart(t.Context(), req); yes || err == nil {
		t.Fatal("revoked owner reused claim")
	}
}
