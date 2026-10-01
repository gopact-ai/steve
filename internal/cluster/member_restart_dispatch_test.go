package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// Only the SSH transport is simulated; member authentication, replicated
// claims, machine-slot admission and restart result tracking remain real.
type restartWireRunner struct{ scripts atomic.Int32 }
type restartWireConnection struct{ runner *restartWireRunner }

func (r *restartWireRunner) Bind(context.Context, string, []string) (sshconnect.Connection, error) {
	return restartWireConnection{r}, nil
}
func (r *restartWireRunner) Run(_ context.Context, _ []string, input string) (sshconnect.Output, error) {
	if strings.Contains(input, "STEVE_CHECK") {
		var probe strings.Builder
		for key, value := range map[string]string{"os": "Linux", "arch": "x86_64", "user": "fixture-user", "address": "192.0.2.8", "bash": "1", "curl": "1", "git": "1", "nohup": "1", "sha256sum": "1", "shasum": "0", "base64": "1", "node": "1", "npm": "1", "existing": "1", "existing_path_peer_state": "~/.steve-peer"} {
			fmt.Fprintf(&probe, "STEVE_CHECK\t%s\t%s\n", key, value)
		}
		for _, candidate := range agenttools.Catalog() {
			fmt.Fprintf(&probe, "STEVE_CHECK\t%s\t0\n", candidate.ID)
		}
		return sshconnect.Output{Stdout: probe.String()}, nil
	}
	if strings.Contains(input, "STEVE_RESTART") {
		r.scripts.Add(1)
		return sshconnect.Output{Stdout: "STEVE_RESTART\trestarted\n"}, nil
	}
	return sshconnect.Output{}, nil
}
func (r *restartWireRunner) Upload(ctx context.Context, args []string, input io.Reader) (sshconnect.Output, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return sshconnect.Output{}, err
	}
	return r.Run(ctx, args, string(raw))
}
func (c restartWireConnection) Run(ctx context.Context, _ string, input string) (sshconnect.Output, error) {
	return c.runner.Run(ctx, nil, input)
}
func (c restartWireConnection) Upload(ctx context.Context, _ string, input io.Reader) (sshconnect.Output, error) {
	return c.runner.Upload(ctx, nil, input)
}
func (c restartWireConnection) Close() error { return nil }

func TestMemberRestartDispatchUsesHolderAndNeverReplaysAfterLostMemory(t *testing.T) {
	hub := startTestHub(t)
	holder := joinNonvoter(t, hub, nil)
	target := joinNonvoter(t, hub, nil)
	active := WaitPeerReady(t, hub)
	cfg := &config.Config{Gateway: config.Gateway{OwnerID: "test-owner", HomePath: "/fixture/home"}}
	if _, err := platformconfig.New(active.Ledger).Bootstrap(t.Context(), cfg, platformconfig.LocalNode{ID: hub.Config.NodeID, Config: config.Node{Addr: hub.Worker().Address, Token: hub.Worker().Token}}); err != nil {
		t.Fatal(err)
	}
	sshConfig := filepath.Join(t.TempDir(), "ssh-config")
	if err := os.WriteFile(sshConfig, []byte("Host fixture-target\n HostName 192.0.2.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &restartWireRunner{}
	fresh := func() *sshconnect.Service {
		return sshconnect.New(sshconnect.Options{ConfigPath: sshConfig, Runner: runner, Backend: peerSSHBackend{peer: holder}, InstallationMode: sshconnect.InstallPeer})
	}
	holder.Mu.Lock()
	holder.Config.Links = map[string]PeerLink{target.Config.NodeID: {Alias: "fixture-target"}}
	holder.localSSH = fresh()
	holder.Mu.Unlock()
	selection, err := hub.MemberRestarts(active).Find(t.Context(), target.Config.NodeID, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	op := attempt.ForceRestart{Selection: selection.Selection, ID: "fixture-restart", ClusterID: hub.Config.ClusterID, NodeID: target.Config.NodeID, Holder: holder.Config.NodeID, By: "test-owner", RequestedAt: time.Now().UTC()}
	if err := active.Ledger.PutBinding(t.Context(), "force-stop-member-restart", op.NodeID, op); err != nil {
		t.Fatal(err)
	}
	control := hub.MemberRestarts(active)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	if err := control.Start(ctx, op); err != nil {
		t.Fatal(err)
	}
	stored, found, err := attempt.New(active.Ledger).ForceRestart(ctx, op.NodeID)
	if err != nil || !found || stored.PlanID == "" || stored.ClaimedAt.IsZero() {
		t.Fatalf("started before a durable claim: %+v %v", stored, err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := control.Status(ctx, stored)
		if err != nil {
			t.Fatal(err)
		}
		if result.State == "connected" {
			break
		}
		if result.State == "failed" {
			local, _ := holder.localSSH.RestartStatus(ctx, op.NodeID)
			t.Fatalf("holder restart failed: %+v details=%+v", result, local.Restart)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if runner.scripts.Load() != 1 {
		t.Fatalf("scripts=%d", runner.scripts.Load())
	}
	if err := control.Start(ctx, op); err != nil {
		t.Fatal(err)
	}
	transport, origin, err := hub.remoteTransport(hub.Runtime.Load().Status().Members[holder.Config.NodeID])
	if err != nil {
		t.Fatal(err)
	}
	requestBody, _ := json.Marshal(memberRestartRequest{Operation: op, Authority: nodewire.SessionAuthority{ClusterID: hub.Config.ClusterID, CoordinatorNodeID: hub.Config.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.String()+clusterMemberRestartPath, strings.NewReader(string(requestBody)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+hub.OwnerToken)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("restart acceptance status/type: %d %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	holder.Mu.Lock()
	previous := holder.localSSH
	holder.localSSH = nil
	holder.Mu.Unlock()
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	holder.Mu.Lock()
	holder.localSSH = fresh()
	holder.Mu.Unlock()
	if err := control.Start(ctx, op); err == nil {
		t.Fatal("reconstructed holder accepted an old claimed restart")
	}
	if runner.scripts.Load() != 1 {
		t.Fatal("old request replayed SSH after holder memory loss")
	}
	result, err := control.Status(ctx, stored)
	if err != nil || result.State != "lost" {
		t.Fatalf("lost state not explicit: %+v %v", result, err)
	}
}
