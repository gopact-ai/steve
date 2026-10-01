package cluster

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

type heldRestartWireRunner struct {
	*restartWireRunner
	entered          chan struct{}
	release          chan struct{}
	once             sync.Once
	preflightEntered chan struct{}
	preflightRelease chan struct{}
	preflightOnce    sync.Once
}

func (r *heldRestartWireRunner) Run(ctx context.Context, args []string, input string) (sshconnect.Output, error) {
	if strings.Contains(input, "STEVE_CHECK") && r.preflightEntered != nil {
		r.preflightOnce.Do(func() { close(r.preflightEntered) })
		select {
		case <-r.preflightRelease:
		case <-ctx.Done():
			return sshconnect.Output{}, ctx.Err()
		}
	}
	if strings.Contains(input, "STEVE_RESTART") {
		r.scripts.Add(1)
		r.once.Do(func() { close(r.entered) })
		if r.release != nil {
			select {
			case <-r.release:
			case <-ctx.Done():
				return sshconnect.Output{}, ctx.Err()
			}
		}
		return sshconnect.Output{Stdout: "STEVE_RESTART\trestarted\n"}, nil
	}
	return r.restartWireRunner.Run(ctx, args, input)
}

func (r *heldRestartWireRunner) Bind(context.Context, string, []string) (sshconnect.Connection, error) {
	return heldRestartWireConnection{r}, nil
}

type heldRestartWireConnection struct{ runner *heldRestartWireRunner }

func (c heldRestartWireConnection) Run(ctx context.Context, _ string, input string) (sshconnect.Output, error) {
	return c.runner.Run(ctx, nil, input)
}
func (c heldRestartWireConnection) Upload(ctx context.Context, _ string, input io.Reader) (sshconnect.Output, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return sshconnect.Output{}, err
	}
	return c.runner.Run(ctx, nil, string(raw))
}
func (c heldRestartWireConnection) Close() error { return nil }

func TestForceRestartJoinsWorkOnAnotherLinkHolder(t *testing.T) {
	control, target, holders := discoveryHolders(t)
	hub := control.(memberRestarts).peer
	active := WaitPeerReady(t, hub)
	sort.Slice(holders, func(i, j int) bool { return holders[i].Config.NodeID < holders[j].Config.NodeID })
	idle, busy := holders[0], holders[1]
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	sshConfig := filepath.Join(t.TempDir(), "ssh-config")
	if err := os.WriteFile(sshConfig, []byte("Host fixture-target\n HostName 192.0.2.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	idleRunner := &heldRestartWireRunner{restartWireRunner: &restartWireRunner{}, entered: make(chan struct{})}
	busyRunner := &heldRestartWireRunner{restartWireRunner: &restartWireRunner{}, entered: make(chan struct{}), release: make(chan struct{})}
	for i, holder := range holders {
		runner := idleRunner
		if i == 1 {
			runner = busyRunner
		}
		holder.Mu.Lock()
		holder.localSSH = sshconnect.New(sshconnect.Options{ConfigPath: sshConfig, Runner: runner, Backend: peerSSHBackend{peer: holder}, InstallationMode: sshconnect.InstallPeer})
		holder.Mu.Unlock()
	}
	manualDone := make(chan error, 1)
	manualExited := make(chan struct{})
	go func() {
		defer close(manualExited)
		_, err := busy.localSSH.Restart(ctx, target)
		manualDone <- err
	}()
	defer func() {
		close(busyRunner.release)
		cancel()
		select {
		case <-manualExited:
		case <-time.After(5 * time.Second):
			t.Error("manual restart did not stop")
		}
	}()
	select {
	case <-busyRunner.entered:
	case err := <-manualDone:
		t.Fatalf("manual operation returned before reaching SSH: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	manual, err := busy.localSSH.RestartStatus(ctx, target)
	if err != nil || manual.Restart == nil {
		t.Fatalf("manual restart not active: %+v %v", manual, err)
	}
	chosen, err := control.Find(ctx, target, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	op := attempt.ForceRestart{Selection: chosen.Selection, ID: "join-existing-restart", ClusterID: chosen.ClusterID, NodeID: target, Holder: chosen.Holder, By: "test-owner", RequestedAt: time.Now().UTC()}
	if err := active.Ledger.PutBinding(ctx, "force-stop-member-restart", target, op); err != nil {
		t.Fatal(err)
	}
	if err := control.Start(ctx, op); err != nil {
		t.Fatal(err)
	}
	stored, _, err := attempt.New(active.Ledger).ForceRestart(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Holder != busy.Config.NodeID {
		select {
		case <-idleRunner.entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if chosen.Holder != busy.Config.NodeID || stored.PlanID != manual.Restart.PlanID || idleRunner.scripts.Load() != 0 {
		t.Fatalf("another holder launched a second operation: selected=%s idle=%s active=%s force-plan=%s manual-plan=%s extra-scripts=%d", chosen.Holder, idle.Config.NodeID, busy.Config.NodeID, stored.PlanID, manual.Restart.PlanID, idleRunner.scripts.Load())
	}
}

func TestMemberRestartRechecksUniqueOwnershipAfterPreflight(t *testing.T) {
	control, target, holders := discoveryHolders(t)
	hub := control.(memberRestarts).peer
	active := WaitPeerReady(t, hub)
	holders[1].Mu.Lock()
	holders[1].Config.Links = nil
	holders[1].Mu.Unlock()
	runner := &heldRestartWireRunner{restartWireRunner: &restartWireRunner{}, entered: make(chan struct{}), preflightEntered: make(chan struct{}), preflightRelease: make(chan struct{})}
	sshConfig := filepath.Join(t.TempDir(), "ssh-config")
	if err := os.WriteFile(sshConfig, []byte("Host fixture-target\n HostName 192.0.2.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	holders[0].Mu.Lock()
	holders[0].localSSH = sshconnect.New(sshconnect.Options{ConfigPath: sshConfig, Runner: runner, Backend: peerSSHBackend{peer: holders[0]}, InstallationMode: sshconnect.InstallPeer})
	holders[0].Mu.Unlock()
	released := false
	defer func() {
		if !released {
			close(runner.preflightRelease)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	targetChoice, err := control.Find(ctx, target, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	op := attempt.ForceRestart{Selection: targetChoice.Selection, ID: "unique-before-preflight", ClusterID: targetChoice.ClusterID, NodeID: target, Holder: targetChoice.Holder, By: "test-owner", RequestedAt: time.Now().UTC()}
	if err := active.Ledger.PutBinding(ctx, "force-stop-member-restart", target, op); err != nil {
		t.Fatal(err)
	}
	if err := control.Start(ctx, op); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.preflightEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	holders[1].Mu.Lock()
	holders[1].Config.Links = map[string]PeerLink{target: {Alias: "another-target-link"}}
	holders[1].Mu.Unlock()
	close(runner.preflightRelease)
	released = true
	stored, _, err := attempt.New(active.Ledger).ForceRestart(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := control.Status(ctx, stored)
		if err != nil {
			t.Fatal(err)
		}
		if result.State == "failed" {
			break
		}
		if result.State == "connected" {
			t.Fatal("stale unique ownership authorized a second holder")
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if runner.scripts.Load() != 0 {
		t.Fatalf("ownership changed during preflight but script ran %d times", runner.scripts.Load())
	}
}
