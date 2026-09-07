package coordination

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

type command struct {
	Kind                       string               `json:"kind"`
	ID                         string               `json:"id"`
	Fingerprint                string               `json:"fingerprint"`
	Actor                      string               `json:"actor"`
	Time                       time.Time            `json:"time"`
	ClusterID                  string               `json:"cluster_id"`
	Member                     Member               `json:"member,omitempty"`
	Transfer                   TransferRequest      `json:"transfer,omitempty"`
	Policy                     PolicyRequest        `json:"policy,omitempty"`
	Eligibility                EligibilityRequest   `json:"eligibility,omitempty"`
	Remove                     RemoveRequest        `json:"remove,omitempty"`
	Address                    MemberAddressRequest `json:"address,omitempty"`
	App                        AppCommand           `json:"app,omitempty"`
	Writer                     WriterRequest        `json:"writer,omitempty"`
	Automatic                  bool                 `json:"automatic,omitempty"`
	ExpectedAppVersion         uint64               `json:"expected_app_version,omitempty"`
	ExpectedConfigurationIndex uint64               `json:"expected_configuration_index,omitempty"`
}

type receipt struct {
	Fingerprint string `json:"fingerprint"`
	Result      Result `json:"result"`
	Code        string `json:"code,omitempty"`
	Message     string `json:"message,omitempty"`
}

func (r receipt) err() error {
	var kind error
	switch r.Code {
	case "":
		return nil
	case "conflict":
		kind = ErrConflict
	case "epoch":
		kind = ErrStaleEpoch
	case "writer":
		kind = ErrStaleWriter
	case "coordinator":
		kind = ErrNotCoordinator
	case "command":
		kind = ErrCommandConflict
	case "application":
		kind = ErrApplication
	default:
		kind = ErrInvalid
	}
	return fmt.Errorf("%w: %s", kind, r.Message)
}

type machine struct {
	mu                sync.RWMutex
	state             State
	receipts          map[string]receipt
	app               Application
	failure           error
	failed            chan struct{}
	membershipChanged chan struct{}
}

func newMachine(clusterID string, app Application) *machine {
	return &machine{state: State{ClusterID: clusterID, Members: map[string]Member{}, Voters: map[string]string{}, Removing: map[string]bool{}, PendingAddresses: map[string]MemberAddressRequest{}}, receipts: map[string]receipt{}, app: app, failed: make(chan struct{}), membershipChanged: make(chan struct{}, 1)}
}

func (m *machine) read() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneState(m.state)
}

func cloneState(s State) State {
	s.Members = maps.Clone(s.Members)
	s.Voters = maps.Clone(s.Voters)
	s.Removing = maps.Clone(s.Removing)
	s.PendingAddresses = maps.Clone(s.PendingAddresses)
	s.Audit = slices.Clone(s.Audit)
	return s
}

func (m *machine) lookup(id, fingerprint string) (receipt, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.receipts[id]
	if ok && r.Fingerprint != fingerprint {
		return receipt{Code: "command", Message: "command ID belongs to different input"}, true
	}
	r.Result.Data = bytes.Clone(r.Result.Data)
	return r, ok
}

func (m *machine) healthy() bool { m.mu.RLock(); defer m.mu.RUnlock(); return m.failure == nil }

func (m *machine) fail(err error) receipt {
	if m.failure == nil {
		m.failure = err
		close(m.failed)
	}
	return receipt{Code: "application", Message: "replicated application failed; replica stopped"}
}

func (m *machine) Apply(log *raft.Log) interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return receipt{Code: "application", Message: "replica is stopped"}
	}
	var c command
	if err := json.Unmarshal(log.Data, &c); err != nil {
		return m.fail(fmt.Errorf("decode committed command: %w", err))
	}
	if c.ClusterID != m.state.ClusterID {
		return m.fail(fmt.Errorf("committed command belongs to another cluster"))
	}
	m.state.AppliedIndex = log.Index
	if old, ok := m.receipts[c.ID]; ok {
		if old.Fingerprint != c.Fingerprint {
			return receipt{Code: "command", Message: "command ID belongs to different input"}
		}
		old.Result.Data = bytes.Clone(old.Result.Data)
		return old
	}
	r := receipt{Fingerprint: c.Fingerprint}
	reject := func(code, message string) { r.Code = code; r.Message = message }
	var event *AuditRecord
	s := &m.state
	switch c.Kind {
	case "initialize":
		if s.Coordinator.Epoch != 0 {
			reject("conflict", "cluster is already initialized")
			break
		}
		if s.Voters[c.Member.NodeID] != c.Member.Address {
			reject("invalid", "initial node is not a voter")
			break
		}
		s.Members[c.Member.NodeID] = c.Member
		s.Coordinator = Assignment{NodeID: c.Member.NodeID, Epoch: 1}
		event = &AuditRecord{Kind: "coordinator_initialized", To: c.Member.NodeID, Epoch: 1}
	case "join_prepare":
		if c.Member.StorageLevel != "restricted" && c.Member.StorageLevel != "sealed" {
			reject("invalid", "full ledger replica requires restricted storage authorization")
			break
		}
		if s.Removing[c.Member.NodeID] {
			reject("conflict", "member removal is in progress")
			break
		}
		if old, ok := s.Members[c.Member.NodeID]; ok {
			if old != c.Member {
				reject("conflict", "member already exists with different properties")
			}
			break
		}
		for _, old := range s.Members {
			if old.Address == c.Member.Address {
				reject("conflict", "address already belongs to another node")
				break
			}
			if c.Member.FailureDomain != "" && old.FailureDomain == c.Member.FailureDomain {
				reject("invalid", "another member already occupies this physical failure domain")
				break
			}
		}
		if r.Code == "" {
			s.Members[c.Member.NodeID] = c.Member
		}
	case "join":
		if s.Voters[c.Member.NodeID] != c.Member.Address {
			reject("conflict", "member has not joined the voting configuration")
			break
		}
		event = &AuditRecord{Kind: "member_joined", To: c.Member.NodeID}
	case "remove_prepare":
		if s.Coordinator.NodeID == c.Remove.NodeID {
			reject("conflict", "current coordinator cannot be removed")
			break
		}
		if _, ok := s.Members[c.Remove.NodeID]; !ok {
			reject("invalid", "member does not exist")
			break
		}
		s.Removing[c.Remove.NodeID] = true
		delete(s.PendingAddresses, c.Remove.NodeID)
	case "remove":
		if _, ok := s.Voters[c.Remove.NodeID]; ok {
			reject("conflict", "member is still in the voting configuration")
			break
		}
		if s.Coordinator.NodeID == c.Remove.NodeID {
			reject("conflict", "current coordinator cannot be removed")
			break
		}
		delete(s.Members, c.Remove.NodeID)
		delete(s.Removing, c.Remove.NodeID)
		delete(s.PendingAddresses, c.Remove.NodeID)
		event = &AuditRecord{Kind: "member_removed", From: c.Remove.NodeID}
	case "address_prepare":
		request := c.Address
		if s.Revision != request.ExpectedRevision {
			reject("conflict", "member address revision changed")
			break
		}
		if _, ok := s.Members[request.NodeID]; !ok || s.Voters[request.NodeID] == "" || s.Removing[request.NodeID] {
			reject("invalid", "address changes require an active voting member")
			break
		}
		if pending, ok := s.PendingAddresses[request.NodeID]; ok && pending.ID != request.ID {
			reject("conflict", "another address update is pending")
			break
		}
		for id, member := range s.Members {
			if id != request.NodeID && (member.Address == request.Address || member.APIAddress == request.APIAddress) {
				reject("conflict", "address belongs to another member")
				break
			}
		}
		if r.Code != "" {
			break
		}
		s.PendingAddresses[request.NodeID] = request
	case "address":
		request := c.Address
		if pending, ok := s.PendingAddresses[request.NodeID]; !ok || pending != request {
			reject("conflict", "address update preparation differs")
			break
		}
		if s.Voters[request.NodeID] != request.Address {
			reject("conflict", "voting configuration has not adopted the new address")
			break
		}
		member := s.Members[request.NodeID]
		event = &AuditRecord{Kind: "member_address_changed", From: request.NodeID, To: request.NodeID, Reason: fmt.Sprintf("Raft %s -> %s; HTTPS %s -> %s", member.Address, request.Address, member.APIAddress, request.APIAddress)}
		member.Address = request.Address
		member.APIAddress = request.APIAddress
		s.Members[request.NodeID] = member
		delete(s.PendingAddresses, request.NodeID)
	case "policy":
		if c.Policy.ExpectedRevision != s.Revision {
			reject("conflict", "policy revision changed")
			break
		}
		if c.Policy.Enabled && !s.CanAutoFailover() {
			reject("invalid", "automatic failover needs at least three voters and two eligible nodes")
			break
		}
		s.AutoFailover = c.Policy.Enabled
		event = &AuditRecord{Kind: "automatic_failover_disabled"}
		if c.Policy.Enabled {
			event.Kind = "automatic_failover_enabled"
		}
	case "eligibility":
		if c.Eligibility.ExpectedRevision != s.Revision {
			reject("conflict", "membership revision changed")
			break
		}
		member, ok := s.Members[c.Eligibility.NodeID]
		if !ok || s.Voters[member.NodeID] == "" {
			reject("invalid", "node is not a voting member")
			break
		}
		member.AutoEligible = c.Eligibility.Eligible
		s.Members[member.NodeID] = member
		event = &AuditRecord{Kind: "automatic_eligibility_removed", To: member.NodeID}
		if member.AutoEligible {
			event.Kind = "automatic_eligibility_granted"
		}
	case "transfer":
		if c.Transfer.ExpectedEpoch != s.Coordinator.Epoch {
			reject("epoch", "coordinator assignment changed")
			break
		}
		if c.ExpectedAppVersion != s.AppVersion || c.ExpectedConfigurationIndex != s.ConfigurationIndex {
			reject("conflict", "replicated state changed while preparing transfer")
			break
		}
		member, ok := s.Members[c.Transfer.TargetNodeID]
		if !ok || s.Voters[member.NodeID] == "" || s.Removing[member.NodeID] {
			reject("invalid", "target is not a voting member")
			break
		}
		if c.Automatic && (!s.AutoFailover || !s.CanAutoFailover() || !member.AutoEligible) {
			reject("invalid", "automatic transfer is not permitted")
			break
		}
		if s.Coordinator.NodeID == member.NodeID {
			reject("invalid", "target already coordinates this cluster")
			break
		}
		event = &AuditRecord{Kind: "coordinator_transferred", From: s.Coordinator.NodeID, To: member.NodeID, Epoch: s.Coordinator.Epoch + 1, Reason: c.Transfer.Reason}
		s.Coordinator = Assignment{NodeID: member.NodeID, Epoch: s.Coordinator.Epoch + 1}
	case "writer":
		if c.Writer.CoordinatorEpoch != s.Coordinator.Epoch {
			reject("epoch", "coordinator assignment changed")
			break
		}
		if c.Writer.CallerNodeID != s.Coordinator.NodeID {
			reject("coordinator", "caller does not hold the coordinator role")
			break
		}
		if c.Writer.ExpectedGeneration != s.WriterGeneration {
			reject("conflict", "writer generation changed")
			break
		}
		if s.WriterGeneration == ^uint64(0) {
			reject("invalid", "writer generation exhausted")
			break
		}
		s.WriterGeneration++
	case "app":
		if c.App.CoordinatorEpoch != s.Coordinator.Epoch {
			reject("epoch", "coordinator assignment changed")
			break
		}
		if c.App.CallerNodeID != s.Coordinator.NodeID {
			reject("coordinator", "caller does not hold the coordinator role")
			break
		}
		if c.App.WriterGeneration == 0 || c.App.WriterGeneration != s.WriterGeneration {
			reject("writer", "writer generation changed")
			break
		}
		if c.App.ExpectedVersion != s.AppVersion {
			reject("conflict", "application version changed")
			break
		}
		if m.app == nil {
			return m.fail(fmt.Errorf("application command received without an application replica"))
		}
		data, err := m.app.Apply(AppliedCommand{ID: c.App.ID, RaftIndex: log.Index, Version: s.AppVersion + 1, Payload: bytes.Clone(c.App.Payload)})
		if err != nil {
			return m.fail(fmt.Errorf("apply index %d: %w", log.Index, err))
		}
		s.AppVersion++
		r.Result.Data = bytes.Clone(data)
	default:
		reject("invalid", "unknown command")
	}
	if r.Code == "" {
		if c.Kind != "app" && c.Kind != "writer" {
			s.Revision++
		}
		if c.Kind == "initialize" || c.Kind == "join_prepare" || c.Kind == "join" || c.Kind == "remove" || c.Kind == "address" {
			m.notifyMembership()
		}
		if event != nil {
			event.CommandID = c.ID
			event.Index = log.Index
			event.Time = c.Time
			event.Actor = c.Actor
			s.Audit = append(s.Audit, *event)
		}
	}
	r.Result.Revision = s.Revision
	r.Result.Index = log.Index
	r.Result.Coordinator = s.Coordinator
	r.Result.AppVersion = s.AppVersion
	r.Result.WriterGeneration = s.WriterGeneration
	m.receipts[c.ID] = r
	r.Result.Data = bytes.Clone(r.Result.Data)
	return r
}

func (m *machine) notifyMembership() {
	select {
	case m.membershipChanged <- struct{}{}:
	default:
	}
}

func (m *machine) StoreConfiguration(index uint64, c raft.Configuration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return
	}
	m.state.ConfigurationIndex = index
	m.state.AppliedIndex = index
	m.state.Revision++
	m.state.Voters = make(map[string]string, len(c.Servers))
	for _, server := range c.Servers {
		if server.Suffrage == raft.Voter {
			m.state.Voters[string(server.ID)] = string(server.Address)
		}
	}
	m.notifyMembership()
}

type snapshotData struct {
	Format         int                `json:"format"`
	State          State              `json:"state"`
	Receipts       map[string]receipt `json:"receipts"`
	HasApplication bool               `json:"has_application"`
	Application    []byte             `json:"application,omitempty"`
}

func (m *machine) Snapshot() (raft.FSMSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.failure != nil {
		return nil, fmt.Errorf("%w: replica stopped", ErrApplication)
	}
	data := snapshotData{Format: 1, State: m.state, Receipts: m.receipts, HasApplication: m.app != nil}
	if m.app != nil {
		var err error
		data.Application, err = m.app.Snapshot()
		if err != nil {
			return nil, fmt.Errorf("snapshot application: %w", err)
		}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &encodedSnapshot{data: encoded}, nil
}

func (m *machine) Restore(reader io.ReadCloser) error {
	defer reader.Close()
	var data snapshotData
	if err := json.NewDecoder(reader).Decode(&data); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if data.Format != 1 || data.State.ClusterID != m.state.ClusterID || data.State.Members == nil || data.State.Voters == nil || data.State.Removing == nil || data.State.PendingAddresses == nil || data.Receipts == nil {
		return fmt.Errorf("%w: snapshot identity or format differs", ErrInvalid)
	}
	if data.HasApplication != (m.app != nil) {
		return fmt.Errorf("%w: snapshot application configuration differs", ErrInvalid)
	}
	if m.app != nil {
		if err := m.app.Restore(data.Application); err != nil {
			m.fail(fmt.Errorf("restore application: %w", err))
			return fmt.Errorf("restore application: %w", err)
		}
	}
	m.state = data.State
	m.receipts = data.Receipts
	m.notifyMembership()
	return nil
}

type encodedSnapshot struct{ data []byte }

func (s *encodedSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}
func (s *encodedSnapshot) Release() {}
