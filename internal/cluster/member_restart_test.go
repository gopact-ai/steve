package cluster

import (
	"context"
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
