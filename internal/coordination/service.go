package coordination

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

// Service is one durable cluster replica. Mutations and ReadState must be sent
// to the consensus leader, which can differ from the business coordinator.
type Service struct {
	config       Config
	raft         *raft.Raft
	transport    *raft.NetworkTransport
	store        *raftboltdb.BoltStore
	fsm          *machine
	opMu         sync.Mutex
	membershipMu sync.Mutex
	closed       atomic.Bool
	closeOnce    sync.Once
	closeErr     error
	ctx          context.Context
	cancel       context.CancelFunc
	workers      sync.WaitGroup
}

// addressTransport keeps RPC source addresses consistent with live listener
// advertisement changes while retaining the same Raft node and log.
type addressTransport struct {
	*raft.NetworkTransport
	nodeID raft.ServerID
}

func (t addressTransport) EncodePeer(id raft.ServerID, address raft.ServerAddress) []byte {
	if id == t.nodeID {
		address = t.LocalAddr()
	}
	return t.NetworkTransport.EncodePeer(id, address)
}

func Open(config Config) (*Service, error) {
	if strings.TrimSpace(config.ClusterID) == "" || strings.TrimSpace(config.NodeID) == "" || config.DataDir == "" {
		return nil, fmt.Errorf("%w: cluster ID, node ID and data directory are required", ErrInvalid)
	}
	if config.BindAddress == "" {
		config.BindAddress = "127.0.0.1:0"
	}
	if config.ApplyTimeout <= 0 {
		config.ApplyTimeout = 5 * time.Second
	}
	if config.FailoverTimeout <= 0 {
		config.FailoverTimeout = 5 * time.Second
	}
	if config.ProbeInterval <= 0 {
		config.ProbeInterval = time.Second
	}
	if config.LogOutput == nil {
		config.LogOutput = io.Discard
	}
	if err := os.MkdirAll(config.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("coordination directory: %w", err)
	}
	if err := checkIdentity(config); err != nil {
		return nil, err
	}
	var transport *raft.NetworkTransport
	if config.StreamLayer != nil {
		transport = raft.NewNetworkTransport(config.StreamLayer, 3, config.ApplyTimeout, config.LogOutput)
	} else {
		if err := requireLoopback(config.BindAddress); err != nil {
			return nil, err
		}
		var advertise net.Addr
		if config.AdvertiseAddress != "" {
			if err := requireLoopback(config.AdvertiseAddress); err != nil {
				return nil, err
			}
			addr, err := net.ResolveTCPAddr("tcp", config.AdvertiseAddress)
			if err != nil {
				return nil, err
			}
			advertise = addr
		}
		var err error
		transport, err = raft.NewTCPTransport(config.BindAddress, advertise, 3, config.ApplyTimeout, config.LogOutput)
		if err != nil {
			return nil, fmt.Errorf("raft transport: %w", err)
		}
	}
	store, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(config.DataDir, "raft.db"), BoltOptions: &bbolt.Options{Timeout: config.ApplyTimeout}})
	if err != nil {
		transport.Close()
		return nil, fmt.Errorf("raft durable store: %w", err)
	}
	snapshots, err := raft.NewFileSnapshotStore(config.DataDir, 2, config.LogOutput)
	if err != nil {
		store.Close()
		transport.Close()
		return nil, fmt.Errorf("raft snapshots: %w", err)
	}
	hasState, err := raft.HasExistingState(store, store, snapshots)
	if err != nil {
		store.Close()
		transport.Close()
		return nil, fmt.Errorf("check raft state: %w", err)
	}
	cfg := raft.DefaultConfig()
	if config.RaftConfig != nil {
		*cfg = *config.RaftConfig
	}
	// An application can contain durable initial data before its first command.
	// Fresh members must install that baseline snapshot, so do not retain a log
	// prefix that would let them catch up solely by replaying later commands.
	if config.Application != nil {
		cfg.TrailingLogs = 0
	}
	cfg.LocalID = raft.ServerID(config.NodeID)
	cfg.LogOutput = config.LogOutput
	fsm := newMachine(config.ClusterID, config.Application)
	node, err := raft.NewRaft(cfg, fsm, store, store, snapshots, addressTransport{NetworkTransport: transport, nodeID: cfg.LocalID})
	if err != nil {
		store.Close()
		transport.Close()
		return nil, fmt.Errorf("open raft: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{config: config, raft: node, transport: transport, store: store, fsm: fsm, ctx: ctx, cancel: cancel}
	if config.Bootstrap && !hasState {
		future := node.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{ID: cfg.LocalID, Address: transport.LocalAddr(), Suffrage: raft.Voter}}})
		if err := future.Error(); err != nil {
			node.Shutdown().Error()
			transport.Close()
			store.Close()
			cancel()
			return nil, fmt.Errorf("bootstrap cluster: %w", err)
		}
	}
	s.workers.Add(1)
	go s.run()
	return s, nil
}

func requireLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: invalid Raft address", ErrInvalid)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w: non-loopback Raft transport requires an authenticated StreamLayer", ErrInvalid)
	}
	return nil
}

func checkIdentity(c Config) error {
	type identity struct {
		ClusterID string `json:"cluster_id"`
		NodeID    string `json:"node_id"`
	}
	want := identity{ClusterID: c.ClusterID, NodeID: c.NodeID}
	path := filepath.Join(c.DataDir, "identity.json")
	contents, err := os.ReadFile(path)
	if err == nil {
		var got identity
		if err := json.Unmarshal(contents, &got); err != nil {
			return fmt.Errorf("read node identity: %w", err)
		}
		if got != want {
			return fmt.Errorf("%w: stored cluster or node identity differs", ErrInvalid)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	contents, _ = json.Marshal(want)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create node identity: %w", err)
	}
	if _, err = file.Write(contents); err != nil {
		file.Close()
		return fmt.Errorf("write node identity: %w", err)
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	dir, err := os.Open(c.DataDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Service) Status() Status {
	address, id := s.raft.LeaderWithID()
	healthy := !s.closed.Load() && s.fsm.healthy() && s.raft.State() != raft.Shutdown
	return Status{State: s.fsm.read(), NodeID: s.config.NodeID, Address: string(s.transport.LocalAddr()), LeaderID: string(id), LeaderAddress: string(address), IsLeader: healthy && s.raft.State() == raft.Leader, Healthy: healthy, FailureDomain: s.config.FailureDomain, StorageLevel: s.config.StorageLevel}
}

// TransportPeers reads Raft's durable latest membership before FSM replay has
// caught up. Authentication cannot rely only on an older application snapshot
// when the later Raft configuration already contains peers needed to elect.
func (s *Service) TransportPeers() map[string]string {
	configuration := s.raft.GetConfiguration().Configuration()
	peers := make(map[string]string, len(configuration.Servers))
	for _, server := range configuration.Servers {
		peers[string(server.ID)] = string(server.Address)
	}
	return peers
}

// ReadState performs a quorum-confirmed read. Status is a local, potentially
// stale view and must never be used to authorize execution or a write.
func (s *Service) ReadState(ctx context.Context) (State, error) {
	if err := s.barrier(ctx); err != nil {
		return State{}, err
	}
	return s.fsm.read(), nil
}

func (s *Service) Snapshot(ctx context.Context) error {
	if s.closed.Load() || !s.fsm.healthy() {
		return ErrUnavailable
	}
	return s.wait(ctx, s.raft.Snapshot())
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		s.closeErr = s.raft.Shutdown().Error()
		s.transport.CloseStreams()
		if err := s.transport.Close(); s.closeErr == nil {
			s.closeErr = err
		}
		s.workers.Wait()
		if err := s.store.Close(); s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

func (s *Service) wait(ctx context.Context, future raft.Future) error {
	result := make(chan error, 1)
	go func() { result <- future.Error() }()
	var err error
	select {
	case err = <-result:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return ErrUnavailable
	case <-s.fsm.failed:
		return ErrApplication
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, raft.ErrNotLeader):
		return fmt.Errorf("%w: leader is %s", ErrNotLeader, s.Status().LeaderID)
	case errors.Is(err, raft.ErrLeadershipLost), errors.Is(err, raft.ErrRaftShutdown), errors.Is(err, raft.ErrEnqueueTimeout):
		return fmt.Errorf("%w: %s", ErrUnavailable, err)
	default:
		return err
	}
}

func (s *Service) barrier(ctx context.Context) error {
	if s.closed.Load() {
		return ErrUnavailable
	}
	if !s.fsm.healthy() {
		return ErrApplication
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.ApplyTimeout)
	defer cancel()
	return s.wait(ctx, s.raft.Barrier(s.config.ApplyTimeout))
}

func fingerprint(kind string, input any) string {
	encoded, _ := json.Marshal(input)
	sum := sha256.Sum256(append([]byte(kind+":"), encoded...))
	return hex.EncodeToString(sum[:])
}

func (s *Service) submit(ctx context.Context, c command) (Result, error) {
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Actor) == "" {
		return Result{}, fmt.Errorf("%w: command ID and actor are required", ErrInvalid)
	}
	if err := s.barrier(ctx); err != nil {
		return Result{}, err
	}
	if r, ok := s.fsm.lookup(c.ID, c.Fingerprint); ok {
		return r.Result, r.err()
	}
	c.ClusterID = s.config.ClusterID
	c.Time = time.Now().UTC()
	encoded, err := json.Marshal(c)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.ApplyTimeout)
	defer cancel()
	future := s.raft.Apply(encoded, s.config.ApplyTimeout)
	if err := s.wait(ctx, future); err != nil {
		return Result{}, err
	}
	r, ok := future.Response().(receipt)
	if !ok {
		return Result{}, fmt.Errorf("%w: invalid FSM response", ErrApplication)
	}
	return r.Result, r.err()
}

func (s *Service) ApplyApp(ctx context.Context, request AppCommand) (Result, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.config.Application == nil {
		return Result{}, fmt.Errorf("%w: no application replica is configured", ErrInvalid)
	}
	return s.submit(ctx, command{Kind: "app", ID: request.ID, Actor: request.CallerNodeID, Fingerprint: fingerprint("app", request), App: request})
}

// BeginWriter fences unresolved commands from earlier business activations
// before the coordinator reconstructs its cached application stores.
func (s *Service) BeginWriter(ctx context.Context, request WriterRequest) (Result, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.config.Application == nil {
		return Result{}, fmt.Errorf("%w: no application replica is configured", ErrInvalid)
	}
	return s.submit(ctx, command{Kind: "writer", ID: request.ID, Actor: request.CallerNodeID, Fingerprint: fingerprint("writer", request), Writer: request})
}

func (s *Service) SetAutoFailover(ctx context.Context, request PolicyRequest) (Result, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.submit(ctx, command{Kind: "policy", ID: request.ID, Actor: request.Actor, Fingerprint: fingerprint("policy", request), Policy: request})
}

func (s *Service) SetEligibility(ctx context.Context, request EligibilityRequest) (Result, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.submit(ctx, command{Kind: "eligibility", ID: request.ID, Actor: request.Actor, Fingerprint: fingerprint("eligibility", request), Eligibility: request})
}

func (s *Service) Transfer(ctx context.Context, request TransferRequest) (Result, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.transfer(ctx, request, false)
}

func (s *Service) transfer(ctx context.Context, request TransferRequest, automatic bool) (Result, error) {
	if err := s.barrier(ctx); err != nil {
		return Result{}, err
	}
	fp := fingerprint("transfer", struct {
		Request   TransferRequest
		Automatic bool
	}{request, automatic})
	if old, ok := s.fsm.lookup(request.ID, fp); ok {
		return old.Result, old.err()
	}
	state := s.fsm.read()
	if request.ExpectedEpoch != state.Coordinator.Epoch {
		return Result{}, ErrStaleEpoch
	}
	if request.TargetNodeID == state.Coordinator.NodeID {
		return Result{}, fmt.Errorf("%w: target already coordinates this cluster", ErrInvalid)
	}
	member, ok := state.Members[request.TargetNodeID]
	if !ok || state.Voters[member.NodeID] == "" || state.Removing[member.NodeID] {
		return Result{}, fmt.Errorf("%w: target is not a voting member", ErrInvalid)
	}
	if err := s.waitForProgress(ctx, member, state); err != nil {
		return Result{}, err
	}
	return s.submit(ctx, command{Kind: "transfer", ID: request.ID, Actor: request.Actor, Fingerprint: fp, Transfer: request, Automatic: automatic, ExpectedAppVersion: state.AppVersion, ExpectedConfigurationIndex: state.ConfigurationIndex})
}

func (s *Service) probe(ctx context.Context, member Member) (Progress, error) {
	if member.NodeID == s.config.NodeID {
		return s.Status().Progress(), nil
	}
	if s.config.Probe == nil {
		return Progress{}, fmt.Errorf("%w: remote progress probe is not configured", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.ProbeInterval)
	defer cancel()
	progress, err := s.config.Probe(ctx, member)
	if err != nil {
		return Progress{}, err
	}
	if progress.ClusterID != s.config.ClusterID || progress.NodeID != member.NodeID {
		return Progress{}, fmt.Errorf("%w: target identity differs", ErrInvalid)
	}
	if member.FailureDomain != "" && progress.FailureDomain != member.FailureDomain {
		return Progress{}, fmt.Errorf("%w: peer physical identity changed", ErrInvalid)
	}
	if member.StorageLevel != "" && progress.StorageLevel != member.StorageLevel {
		return Progress{}, fmt.Errorf("%w: peer storage authorization changed", ErrInvalid)
	}
	return progress, nil
}

func (s *Service) waitForProgress(ctx context.Context, member Member, state State) error {
	ctx, cancel := context.WithTimeout(ctx, s.config.ApplyTimeout)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var probeErr error
	for {
		progress, err := s.probe(ctx, member)
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrApplication) {
			return fmt.Errorf("%w: cannot verify target: %v", ErrNotReady, err)
		}
		probeErr = err
		if err == nil && progress.AppliedIndex >= state.AppliedIndex && progress.AppVersion >= state.AppVersion {
			return nil
		}
		select {
		case <-ctx.Done():
			if probeErr != nil {
				return fmt.Errorf("%w: cannot verify target before deadline: %v", ErrNotReady, probeErr)
			}
			return fmt.Errorf("%w: target is behind the committed state", ErrNotReady)
		case <-s.ctx.Done():
			return ErrUnavailable
		case <-ticker.C:
		}
	}
}

// Join first adds a nonvoter and verifies its actual applied progress before
// changing the voting configuration. Interrupted joins can safely be retried
// with the same ID. A failed member is never removed automatically.
func (s *Service) Join(ctx context.Context, request JoinRequest) (Result, error) {
	s.membershipMu.Lock()
	defer s.membershipMu.Unlock()
	if request.Member.NodeID == "" || request.Member.Address == "" || request.ID == "" || request.Actor == "" {
		return Result{}, fmt.Errorf("%w: member identity, address, command ID and actor are required", ErrInvalid)
	}
	if s.config.StreamLayer == nil {
		if err := requireLoopback(request.Member.Address); err != nil {
			return Result{}, err
		}
	}
	if err := s.barrier(ctx); err != nil {
		return Result{}, err
	}
	fp := fingerprint("join", request)
	if old, ok := s.fsm.lookup(request.ID, fp); ok {
		return old.Result, old.err()
	}
	progress, err := s.probe(ctx, request.Member)
	if err != nil {
		return Result{}, fmt.Errorf("verify joining node: %w", err)
	}
	request.Member.FailureDomain = progress.FailureDomain
	request.Member.StorageLevel = progress.StorageLevel
	if request.Member.StorageLevel != "restricted" && request.Member.StorageLevel != "sealed" {
		return Result{}, fmt.Errorf("%w: a full ledger replica requires explicit restricted storage authorization", ErrInvalid)
	}
	if request.Member.FailureDomain == "" {
		return Result{}, fmt.Errorf("%w: joining node has no verified physical failure domain", ErrInvalid)
	}
	if s.config.AuthorizeReplica != nil {
		if err := s.config.AuthorizeReplica(ctx, request.Member); err != nil {
			return Result{}, fmt.Errorf("%w: ledger replica authorization: %v", ErrInvalid, err)
		}
	}
	if _, err := s.submit(ctx, command{Kind: "join_prepare", ID: request.ID + "/prepare", Actor: request.Actor, Fingerprint: fp, Member: request.Member}); err != nil {
		return Result{}, err
	}
	if s.config.Application != nil {
		if err := s.wait(ctx, s.raft.Snapshot()); err != nil && !errors.Is(err, raft.ErrNothingNewToSnapshot) {
			return Result{}, err
		}
	}
	configuration := s.raft.GetConfiguration()
	if err := s.wait(ctx, configuration); err != nil {
		return Result{}, err
	}
	isVoter := false
	present := false
	for _, server := range configuration.Configuration().Servers {
		if string(server.ID) == request.Member.NodeID {
			if string(server.Address) != request.Member.Address {
				return Result{}, fmt.Errorf("%w: member address differs", ErrConflict)
			}
			present = true
			isVoter = server.Suffrage == raft.Voter
		} else if string(server.Address) == request.Member.Address {
			return Result{}, fmt.Errorf("%w: address belongs to another member", ErrConflict)
		}
	}
	if !present {
		if err := s.wait(ctx, s.raft.AddNonvoter(raft.ServerID(request.Member.NodeID), raft.ServerAddress(request.Member.Address), configuration.Index(), s.config.ApplyTimeout)); err != nil {
			return Result{}, err
		}
	}
	if err := s.barrier(ctx); err != nil {
		return Result{}, err
	}
	if err := s.waitForProgress(ctx, request.Member, s.fsm.read()); err != nil {
		return Result{}, err
	}
	if !isVoter {
		if s.config.ValidateJoin != nil {
			if err := s.config.ValidateJoin(ctx, request.Member); err != nil {
				return Result{}, fmt.Errorf("%w: candidate network verification: %v", ErrNotReady, err)
			}
		}
		configuration = s.raft.GetConfiguration()
		if err := s.wait(ctx, configuration); err != nil {
			return Result{}, err
		}
		if err := s.wait(ctx, s.raft.AddVoter(raft.ServerID(request.Member.NodeID), raft.ServerAddress(request.Member.Address), configuration.Index(), s.config.ApplyTimeout)); err != nil {
			return Result{}, err
		}
	}
	return s.submit(ctx, command{Kind: "join", ID: request.ID, Actor: request.Actor, Fingerprint: fp, Member: request.Member})
}

func (s *Service) UpdateMemberAddress(ctx context.Context, request MemberAddressRequest) (Result, error) {
	s.membershipMu.Lock()
	defer s.membershipMu.Unlock()
	if request.ID == "" || request.Actor == "" || request.NodeID == "" {
		return Result{}, ErrInvalid
	}
	if host, port, err := net.SplitHostPort(request.Address); err != nil || host == "" || port == "" {
		return Result{}, fmt.Errorf("%w: invalid member Raft address", ErrInvalid)
	}
	endpoint, err := url.Parse(request.APIAddress)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.User != nil || endpoint.Fragment != "" {
		return Result{}, fmt.Errorf("%w: invalid member HTTPS address", ErrInvalid)
	}
	if err := s.barrier(ctx); err != nil {
		return Result{}, err
	}
	fp := fingerprint("address", request)
	if old, ok := s.fsm.lookup(request.ID, fp); ok {
		return old.Result, old.err()
	}
	state := s.fsm.read()
	member, ok := state.Members[request.NodeID]
	if !ok {
		return Result{}, ErrInvalid
	}
	member.Address = request.Address
	member.APIAddress = request.APIAddress
	if s.config.Probe == nil {
		return Result{}, fmt.Errorf("%w: address verification is not configured", ErrNotReady)
	}
	progress, err := s.config.Probe(ctx, member)
	if err != nil {
		return Result{}, fmt.Errorf("%w: verifying new endpoint: %v", ErrNotReady, err)
	}
	if progress.ClusterID != state.ClusterID || progress.NodeID != member.NodeID || progress.AppliedIndex < state.AppliedIndex || progress.AppVersion < state.AppVersion || progress.FailureDomain != member.FailureDomain || progress.StorageLevel != member.StorageLevel {
		return Result{}, fmt.Errorf("%w: new member endpoint is not a synchronized replica", ErrNotReady)
	}
	if _, err := s.submit(ctx, command{Kind: "address_prepare", ID: request.ID + "/prepare", Actor: request.Actor, Fingerprint: fp, Address: request}); err != nil {
		return Result{}, err
	}
	prepared := s.fsm.read()
	if prepared.Removing[request.NodeID] || prepared.PendingAddresses[request.NodeID] != request {
		return Result{}, ErrConflict
	}
	if s.config.ValidateAddress != nil {
		if err := s.config.ValidateAddress(ctx, member); err != nil {
			return Result{}, fmt.Errorf("%w: member address network verification: %v", ErrNotReady, err)
		}
	}
	configuration := s.raft.GetConfiguration()
	if err := s.wait(ctx, configuration); err != nil {
		return Result{}, err
	}
	if err := s.wait(ctx, s.raft.AddVoter(raft.ServerID(request.NodeID), raft.ServerAddress(request.Address), configuration.Index(), s.config.ApplyTimeout)); err != nil {
		return Result{}, err
	}
	return s.submit(ctx, command{Kind: "address", ID: request.ID, Actor: request.Actor, Fingerprint: fp, Address: request})
}

func (s *Service) Remove(ctx context.Context, request RemoveRequest) (Result, error) {
	s.membershipMu.Lock()
	defer s.membershipMu.Unlock()
	if request.ID == "" || request.Actor == "" || request.NodeID == "" {
		return Result{}, fmt.Errorf("%w: command ID, actor and node ID are required", ErrInvalid)
	}
	if err := s.barrier(ctx); err != nil {
		return Result{}, err
	}
	fp := fingerprint("remove", request)
	if old, ok := s.fsm.lookup(request.ID, fp); ok {
		return old.Result, old.err()
	}
	state := s.fsm.read()
	if request.NodeID == state.Coordinator.NodeID {
		return Result{}, fmt.Errorf("%w: transfer coordination before removing this node", ErrInvalid)
	}
	if request.NodeID == s.config.NodeID {
		var targets []string
		for id := range state.Voters {
			if id != request.NodeID && !state.Removing[id] {
				targets = append(targets, id)
			}
		}
		sort.Strings(targets)
		if len(targets) == 0 {
			return Result{}, fmt.Errorf("%w: no consensus leadership target is available", ErrInvalid)
		}
		for _, target := range targets {
			progress, err := s.probe(ctx, state.Members[target])
			if err != nil || progress.AppliedIndex < state.AppliedIndex || progress.AppVersion < state.AppVersion {
				continue
			}
			if err := s.wait(ctx, s.raft.LeadershipTransferToServer(raft.ServerID(target), raft.ServerAddress(state.Voters[target]))); err != nil {
				return Result{}, err
			}
			return Result{}, ErrNotLeader
		}
		return Result{}, ErrNotReady
	}
	if _, ok := state.Members[request.NodeID]; !ok {
		return Result{}, fmt.Errorf("%w: member does not exist", ErrInvalid)
	}
	if _, err := s.submit(ctx, command{Kind: "remove_prepare", ID: request.ID + "/prepare", Actor: request.Actor, Fingerprint: fp, Remove: request}); err != nil {
		return Result{}, err
	}
	configuration := s.raft.GetConfiguration()
	if err := s.wait(ctx, configuration); err != nil {
		return Result{}, err
	}
	if err := s.wait(ctx, s.raft.RemoveServer(raft.ServerID(request.NodeID), configuration.Index(), s.config.ApplyTimeout)); err != nil {
		return Result{}, err
	}
	return s.submit(ctx, command{Kind: "remove", ID: request.ID, Actor: request.Actor, Fingerprint: fp, Remove: request})
}
