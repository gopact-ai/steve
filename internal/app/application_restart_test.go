package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
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

// A standalone hub owns the children its previous process left behind the
// same way a coordinator does. A delegated child whose accounting row was
// opened, but whose attempt never reached the ledger before the hub died,
// is settled as interrupted once the hub runs again, instead of keeping its
// tree busy with an execution nobody is running.
func TestStandaloneRestartSettlesDelegatedChildThatNeverReachedTheLedger(t *testing.T) {
	installation, err := desktop.Bootstrap(desktop.Options{StateDir: filepath.Join(t.TempDir(), "desktop")})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(installation.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Dir(cfg.Gateway.StatePath)
	// The previous process: the parent delegated, the child's row was
	// opened, and the hub died before the child's attempt was admitted.
	book, err := ledger.Open(stateDir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := tasks.Create(task.Task{Goal: "parent goal", Transport: "console", Channel: "console:standalone-restart", Member: "planner", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(parent.ID, "planner", "", ""); err != nil {
		t.Fatal(err)
	}
	child, err := tasks.Spawn(parent.ID, task.Task{Goal: "child goal", Member: "builder", Node: "node-b", Origin: "delegate:" + parent.ID, ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(child.ID, "builder", "node-b", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Finish(parent.ID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}

	// The hub runs again with no cluster environment.
	application, err := Build(t.Context(), Config{Path: installation.Paths.Config})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	defer func() {
		stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("standalone hub stopped with %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("standalone hub did not stop")
		}
	}()
	running, err := config.Load(installation.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	var detail struct {
		Task struct {
			State task.State `json:"state"`
		} `json:"task"`
		Accounting struct {
			Items []struct {
				Outcome string `json:"outcome"`
			} `json:"items"`
		} `json:"accounting"`
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for deadline := time.Now().Add(20 * time.Second); ; {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+running.Gateway.ReadModelAddr+"/console/tasks/"+child.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+running.Gateway.ReadModelToken)
		if response, err := client.Do(request); err == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK && json.Unmarshal(body, &detail) == nil && detail.Task.State == task.StateFailed {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("child left behind by the previous process was never settled: %+v", detail)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(detail.Accounting.Items) != 1 || detail.Accounting.Items[0].Outcome != string(task.OutcomeInterrupted) {
		t.Fatalf("child's accounting row was not closed as interrupted: %+v", detail.Accounting.Items)
	}
}
