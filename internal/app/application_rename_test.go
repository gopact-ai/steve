package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// A machine keeps its identity when people rename it: the coordination
// view and every /state node row show the new display name against the
// unchanged node ID, and a follower sees the same name.
func TestClusterPeerRenameChangesDisplayNameNotIdentity(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	first := StartTestPeer(t, options)
	WaitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	second := StartTestPeer(t, secondOptions)
	if _, err := first.Join(context.Background(), coordination.JoinRequest{ID: "rename-join", Actor: "owner", Member: coordination.Member{NodeID: second.Config.NodeID, Address: second.Config.RaftAddress, APIAddress: second.Config.PeerURL, Name: "second-host", Voting: true}}); err != nil {
		t.Fatal(err)
	}
	status, body := PeerRequest(t, first, http.MethodGet, "/console/coordination", nil)
	if status != http.StatusOK {
		t.Fatalf("coordination view: %d %s", status, body)
	}
	var view consoleapi.CoordinationView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	status, body = PeerRequest(t, first, http.MethodPut, "/console/coordination/name", consoleapi.CoordinatorRename{CommandID: "rename-local", ExpectedRevision: view.Revision, NodeID: first.Config.NodeID, Name: " 我的 Mac "})
	if status != http.StatusOK {
		t.Fatalf("rename: %d %s", status, body)
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, n := range view.Nodes {
		names[n.ID] = n.Name
	}
	if names[first.Config.NodeID] != "我的 Mac" || names[second.Config.NodeID] != "second-host" {
		t.Fatalf("coordination names after rename: %v", names)
	}
	status, body = PeerRequest(t, first, http.MethodPut, "/console/coordination/name", consoleapi.CoordinatorRename{CommandID: "rename-stale", ExpectedRevision: view.Revision - 1, NodeID: first.Config.NodeID, Name: "stale"})
	if status != http.StatusConflict {
		t.Fatalf("stale rename must conflict: %d %s", status, body)
	}
	status, body = PeerRequest(t, first, http.MethodPut, "/console/coordination/name", consoleapi.CoordinatorRename{CommandID: "rename-blank", ExpectedRevision: view.Revision, NodeID: first.Config.NodeID, Name: "   "})
	if status != http.StatusBadRequest {
		t.Fatalf("blank rename must be invalid: %d %s", status, body)
	}
	status, body = PeerRequest(t, first, http.MethodGet, "/state", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d %s", status, body)
	}
	var snap readmodel.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range snap.Nodes {
		if n.Name == first.Config.NodeID {
			found = true
			if n.DisplayName != "我的 Mac" {
				t.Fatalf("state node row = %+v, want display name without changing the identity", n)
			}
		}
		if n.DisplayName == n.Name {
			t.Fatalf("display name must not duplicate the identity: %+v", n)
		}
	}
	if !found {
		t.Fatalf("local node missing from state: %+v", snap.Nodes)
	}
	deadline := time.Now().Add(5 * time.Second)
	for second.MemberNames()[first.Config.NodeID] != "我的 Mac" {
		if time.Now().After(deadline) {
			t.Fatalf("follower did not replicate the display name: %v", second.MemberNames())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := any(second).(cluster.ApplicationHost); !ok {
		t.Fatal("peer must keep satisfying the application host port")
	}
}
