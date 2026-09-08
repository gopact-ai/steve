package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestClusterApplicationRestartKeepsGatewayWorkerAndDurableReceipt(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	peer := StartTestPeer(t, options)
	first := WaitPeerReady(t, peer)
	worker, origin := peer.Worker(), peer.UiURL
	status, body := PeerRequest(t, peer, http.MethodPost, "/console/services/hub/restart", consoleapi.RestartRequest{CommandID: "restart-cluster-app"})
	if status != http.StatusOK {
		t.Fatalf("restart request: %d %s", status, body)
	}
	var receipt consoleapi.RestartOperation
	if err := json.Unmarshal(body, &receipt); err != nil || receipt.State != "accepted" {
		t.Fatalf("restart receipt: %+v %v", receipt, err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; {
		if peer.Runtime.Load().Status().Closed {
			t.Fatal("application restart stopped the cluster peer")
		}
		status, body = PeerRequest(t, peer, http.MethodGet, "/console/services/hub/restart?command_id=restart-cluster-app", nil)
		if status == http.StatusOK && json.Unmarshal(body, &receipt) == nil && receipt.State == "restarted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart never became ready: %d %s", status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	next := WaitPeerReady(t, peer)
	if peer.UiURL != origin || peer.Worker() != worker || next.Assignment != first.Assignment || next.WriterGeneration <= first.WriterGeneration {
		t.Fatal("application restart changed peer identity or failed to fence old writer")
	}
	status, body = PeerRequest(t, peer, http.MethodPost, "/console/services/hub/restart", consoleapi.RestartRequest{CommandID: "restart-cluster-app"})
	if status != http.StatusOK || json.Unmarshal(body, &receipt) != nil || receipt.State != "restarted" {
		t.Fatalf("restart replay failed: %d %s", status, body)
	}
	if WaitPeerReady(t, peer).WriterGeneration != next.WriterGeneration {
		t.Fatal("receipt replay requested another restart")
	}
}
