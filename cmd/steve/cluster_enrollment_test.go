package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/hashicorp/raft"
)

type enrollmentChildCommand struct {
	Action    string                `json:"action"`
	ID        string                `json:"id,omitempty"`
	Request   PeerEnrollmentRequest `json:"request"`
	Address   NetworkAddressRequest `json:"address"`
	ReplyPath string                `json:"reply_path"`
}
type enrollmentChildReply struct {
	Error      string               `json:"error,omitempty"`
	Plan       PeerEnrollmentPlan   `json:"plan"`
	Payload    []byte               `json:"payload,omitempty"`
	NodeID     string               `json:"node_id,omitempty"`
	Result     PeerEnrollmentResult `json:"result"`
	State      coordination.State   `json:"state"`
	Worker     peerWorkerDescriptor `json:"worker"`
	Generation uint64               `json:"generation"`
}

// The control pipe models owner-confirmed local actions. Replication,
// enrollment verification and worker registration all use actual mTLS sockets
// between three distinct child processes with simulated machine identities.
func TestClusterEnrollmentPeerProcess(t *testing.T) {
	if os.Getenv("STEVE_ENROLLMENT_TEST_HELPER") != "1" {
		return
	}
	configPath := os.Getenv("STEVE_ENROLLMENT_TEST_CONFIG")
	settings, err := loadClusterPeerConfig(defaultClusterConfigPath(configPath))
	if err != nil {
		t.Fatal(err)
	}
	raftConfig := raft.DefaultConfig()
	raftConfig.HeartbeatTimeout = 250 * time.Millisecond
	raftConfig.ElectionTimeout = 250 * time.Millisecond
	raftConfig.LeaderLeaseTimeout = 125 * time.Millisecond
	raftConfig.CommitTimeout = 10 * time.Millisecond
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	peer, err := openClusterPeer(ctx, clusterPeerOptions{ConfigPath: configPath, RaftConfig: raftConfig, PollInterval: 25 * time.Millisecond, TestFailureDomain: func() (string, error) { return "test-process-machine-" + settings.NodeID, nil }})
	if err != nil {
		t.Fatal(err)
	}
	go func() { <-ctx.Done(); os.Stdin.Close() }()
	decoder := json.NewDecoder(os.Stdin)
	for {
		var command enrollmentChildCommand
		if err := decoder.Decode(&command); err != nil {
			break
		}
		result := enrollmentChildReply{}
		callCtx, stop := context.WithTimeout(ctx, 25*time.Second)
		switch command.Action {
		case "ready":
			var activeErr error
			_, activeErr = peer.runtime.Load().WaitReady(callCtx)
			err = activeErr
		case "preview":
			result.Plan, err = peer.previewPeerEnrollment(callCtx, command.Request, true)
		case "prepare":
			var prepared PeerEnrollmentPackage
			prepared, err = peer.preparePeerEnrollment(callCtx, command.Request, command.ID, true)
			result.NodeID, result.Payload = prepared.NodeID, prepared.Payload
			result.Plan = prepared.Plan
		case "complete":
			result.Result, err = peer.CompletePeerEnrollment(callCtx, command.ID)
		case "address":
			_, err = peer.SetNetworkAddress(callCtx, command.Address)
		case "state":
			result.State, err = peer.runtime.Load().ReadState(callCtx)
		default:
			err = errors.New("unknown test command")
		}
		stop()
		if err != nil {
			result.Error = err.Error()
		}
		result.Worker = peer.Worker()
		result.Generation = peer.runtime.Load().Status().Generation
		if err := saveClusterJSON(command.ReplyPath, result, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
}

type enrollmentProcess struct {
	t          *testing.T
	cmd        *exec.Cmd
	input      io.WriteCloser
	done       chan error
	root       string
	configPath string
	next       atomic.Uint64
}

func startEnrollmentProcess(t *testing.T, configPath string) *enrollmentProcess {
	t.Helper()
	logPath := filepath.Join(filepath.Dir(configPath), "test-process.log")
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestClusterEnrollmentPeerProcess$")
	command.Env = append(os.Environ(), "STEVE_ENROLLMENT_TEST_HELPER=1", "STEVE_ENROLLMENT_TEST_CONFIG="+configPath)
	command.Stdout, command.Stderr = output, output
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &enrollmentProcess{t: t, cmd: command, input: input, done: make(chan error, 1), root: filepath.Dir(configPath), configPath: configPath}
	go func() { process.done <- command.Wait(); output.Close() }()
	t.Cleanup(func() {
		input.Close()
		select {
		case err := <-process.done:
			if err != nil {
				data, _ := os.ReadFile(logPath)
				t.Errorf("isolated peer process exited: %v\n%s", err, data)
			}
		case <-time.After(12 * time.Second):
			command.Process.Kill()
			<-process.done
			t.Error("isolated peer did not stop")
		}
	})
	return process
}

func (p *enrollmentProcess) call(command enrollmentChildCommand) enrollmentChildReply {
	p.t.Helper()
	command.ReplyPath = filepath.Join(p.root, fmt.Sprintf("test-reply-%d.json", p.next.Add(1)))
	if err := json.NewEncoder(p.input).Encode(command); err != nil {
		p.t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := readClusterPrivate(command.ReplyPath)
		if err == nil {
			var result enrollmentChildReply
			if err := json.Unmarshal(data, &result); err != nil {
				p.t.Fatal(err)
			}
			os.Remove(command.ReplyPath)
			return result
		}
		if !errors.Is(err, os.ErrNotExist) {
			p.t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(filepath.Join(p.root, "test-process.log"))
	p.t.Fatalf("isolated peer did not answer %s\n%s", command.Action, data)
	return enrollmentChildReply{}
}

func freeEnrollmentPorts(t *testing.T) (string, string) {
	t.Helper()
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	return first.Addr().String(), second.Addr().String()
}

func waitEnrollmentStatus(t *testing.T, sourceConfig string, targetConfig string) {
	t.Helper()
	source, err := loadClusterPeerConfig(defaultClusterConfigPath(sourceConfig))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := source.tlsOptions()
	if err != nil {
		t.Fatal(err)
	}
	client, err := coordination.NewClient(coordination.ClientConfig{TLS: identity, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		target, err := loadClusterPeerConfig(defaultClusterConfigPath(targetConfig))
		if err == nil {
			status, err := client.Status(context.Background(), coordination.Member{NodeID: target.NodeID, APIAddress: target.PeerURL})
			if err == nil && status.Healthy {
				return
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("new isolated peer HTTPS service did not start")
}

func TestPeerEnrollmentThreeProcessesReplicateAndRegisterWorkers(t *testing.T) {
	root := clusterPeerTestDir(t)
	options, _ := testPeerOptions(t, filepath.Join(root, "source"), nil)
	settings, err := loadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	settings.RaftBindAddress = "0.0.0.0:0"
	settings.PeerBindAddress = "0.0.0.0:0"
	if err := saveClusterJSON(options.ClusterPath, settings, false); err != nil {
		t.Fatal(err)
	}
	source := startEnrollmentProcess(t, options.ConfigPath)
	ready := source.call(enrollmentChildCommand{Action: "ready"})
	if ready.Error != "" {
		t.Fatal(ready.Error)
	}
	before := source.call(enrollmentChildCommand{Action: "state"})
	if before.Error != "" {
		t.Fatal(before.Error)
	}
	var changed enrollmentChildReply
	for attempt := 0; attempt < 5; attempt++ {
		latest := source.call(enrollmentChildCommand{Action: "state"})
		if latest.Error != "" {
			t.Fatal(latest.Error)
		}
		changed = source.call(enrollmentChildCommand{Action: "address", Address: NetworkAddressRequest{ID: fmt.Sprintf("source-verified-address-%d", attempt), ExpectedRevision: latest.State.Revision, Host: "localhost"}})
		if !strings.Contains(changed.Error, coordination.ErrConflict.Error()) {
			break
		}
	}
	if changed.Error != "" {
		t.Fatal(changed.Error)
	}
	if changed.Worker != ready.Worker || changed.Generation != ready.Generation {
		t.Fatal("address update restarted the worker or business application")
	}
	var importedNodes []peerImportResult
	var importedProcesses []*enrollmentProcess
	for index, name := range []string{"peer-alpha", "peer-beta"} {
		peerAddress, raftAddress := freeEnrollmentPorts(t)
		request := PeerEnrollmentRequest{Alias: name, Name: name, PeerAddress: peerAddress, RaftAddress: raftAddress, SourceHost: "localhost", Level: "restricted"}
		preview := source.call(enrollmentChildCommand{Action: "preview", Request: request})
		if preview.Error != "" {
			t.Fatal(preview.Error)
		}
		request = preview.Plan.Request
		request.ExpectedPlanHash = preview.Plan.ReviewID
		id := fmt.Sprintf("isolated-join-%d", index)
		prepared := source.call(enrollmentChildCommand{Action: "prepare", ID: id, Request: request})
		if prepared.Error != "" {
			t.Fatal(prepared.Error)
		}
		var bundle peerJoinPackage
		if err := json.Unmarshal(prepared.Payload, &bundle); err != nil {
			t.Fatal(err)
		}
		if len(bundle.PrivateKey) == 0 || len(bundle.CA) == 0 || bundle.NodeID == "" || bundle.ClusterID != before.State.ClusterID {
			t.Fatal("private enrollment did not produce a complete node identity")
		}
		imported, err := importPeerPackage(prepared.Payload, filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := importPeerPackage(prepared.Payload, filepath.Join(root, name))
		if err != nil || repeated != imported {
			t.Fatal("same operation import was not idempotent")
		}
		modified := bundle
		modified.Name = "changed-name"
		different, _ := json.Marshal(modified)
		if _, err := importPeerPackage(different, filepath.Join(root, name)); err == nil {
			t.Fatal("same import ID accepted a different enrollment package")
		}
		if _, err := os.Stat(filepath.Join(root, name, "cluster", "ca-key.pem")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("source CA private key was copied to another node")
		}
		importedProcesses = append(importedProcesses, startEnrollmentProcess(t, imported.ConfigPath))
		waitEnrollmentStatus(t, source.configPath, imported.ConfigPath)
		completed := source.call(enrollmentChildCommand{Action: "complete", ID: id})
		if completed.Error != "" || !completed.Result.Ready {
			t.Fatalf("join did not finish: %s (phase %s)", completed.Error, completed.Result.Phase)
		}
		again := source.call(enrollmentChildCommand{Action: "complete", ID: id})
		if again.Error != "" || again.Result.NodeID != completed.Result.NodeID || len(again.Result.Steps) != len(completed.Result.Steps) {
			t.Fatal("completed enrollment replay was not idempotent")
		}
		importedNodes = append(importedNodes, imported)
	}
	state := source.call(enrollmentChildCommand{Action: "state"})
	if state.Error != "" {
		t.Fatal(state.Error)
	}
	if len(state.State.Voters) != 3 || state.State.AutoFailover {
		t.Fatalf("unexpected membership/policy: %d voters automatic=%v", len(state.State.Voters), state.State.AutoFailover)
	}
	for _, node := range importedNodes {
		member := state.State.Members[node.NodeID]
		if member.AutoEligible || member.FailureDomain == "" {
			t.Fatal("new node was auto eligible or had no physical identity")
		}
	}
	declaration, err := config.Load(source.configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := loadClusterPeerConfig(defaultClusterConfigPath(source.configPath))
	if err != nil {
		t.Fatal(err)
	}
	requestBody := fmt.Sprintf(`{"command_id":"to-enrolled-peer","expected_epoch":%d,"target_node_id":%q}`, state.State.Coordinator.Epoch, importedNodes[0].NodeID)
	request, err := http.NewRequest(http.MethodPost, "http://"+saved.UIAddress+"/console/coordination/transfer", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+declaration.Gateway.ReadModelToken)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("enrolled voting member could not be manually selected: HTTP %d %s", response.StatusCode, data)
	}
	if activated := importedProcesses[0].call(enrollmentChildCommand{Action: "ready"}); activated.Error != "" {
		t.Fatalf("selected peer did not activate its full application: %s", activated.Error)
	}
}

func TestPeerEnrollmentReviewChangesFailBeforeIssuingIdentity(t *testing.T) {
	options, _ := testPeerOptions(t, clusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := startTestPeer(t, options)
	waitPeerReady(t, peer)
	peerAddress, raftAddress := freeEnrollmentPorts(t)
	request := PeerEnrollmentRequest{Name: "reviewed-peer", PeerAddress: peerAddress, RaftAddress: raftAddress, SourceHost: "127.0.0.1", Level: "restricted"}
	plan, err := peer.previewPeerEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	request.Level = "sealed"
	if _, err := peer.preparePeerEnrollment(t.Context(), request, "changed-plan", true); !errors.Is(err, coordination.ErrConflict) {
		t.Fatalf("unreviewed plan change accepted: %v", err)
	}
	if _, err := peer.loadEnrollment("changed-plan"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unreviewed change created a node identity")
	}
}

func TestPeerEnrollmentPhysicalIdentityIsStableAndNotInstallationSpecific(t *testing.T) {
	first, err := physicalFailureDomain()
	if err != nil {
		t.Skip("OS machine identity unavailable")
	}
	second, err := physicalFailureDomain()
	if err != nil || first != second || len(first) != len("machine-")+64 {
		t.Fatal("physical failure domain is not a stable opaque machine hash")
	}
}
