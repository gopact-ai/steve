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

// reject marks the receipt as refused; the state stays as it was.
func (r *receipt) reject(code, message string) { r.Code = code; r.Message = message }

// Apply is the Raft state machine: every replica runs the same committed
// entries in the same order, so nothing in here may depend on anything
// but the entry and the state before it. A command is decoded, checked
// against the receipt of an earlier entry with the same ID, applied by
// its kind, and answered with a receipt that is also recorded for the
// next entry reusing that ID.
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
	event, err := m.applyCommand(c, log.Index, &r)
	if err != nil {
		return m.fail(err)
	}
	return m.settle(c, log.Index, r, event)
}

// applyCommand dispatches one committed command by kind. A rejection is
// written into r and leaves the state untouched; an error is fatal for
// the replica and leaves no receipt behind.
func (m *machine) applyCommand(c command, index uint64, r *receipt) (*AuditRecord, error) {
	s := &m.state
	switch c.Kind {
	case "initialize":
		return applyInitialize(s, c, r), nil
	case "join_prepare":
		return applyJoinPrepare(s, c, r), nil
	case "join":
		return applyJoin(s, c, r), nil
	case "remove_prepare":
		return applyRemovePrepare(s, c, r), nil
	case "remove":
		return applyRemove(s, c, r), nil
	case "address_prepare":
		return applyAddressPrepare(s, c, r), nil
	case "address":
		return applyAddress(s, c, r), nil
	case "policy":
		return applyPolicy(s, c, r), nil
	case "eligibility":
		return applyEligibility(s, c, r), nil
	case "transfer":
		return applyTransfer(s, c, r), nil
	case "writer":
		return applyWriter(s, c, r), nil
	case "app":
		return nil, m.applyApp(c, index, r)
	default:
		r.reject("invalid", "unknown command")
		return nil, nil
	}
}

// settle records what applying c did: an accepted command advances the
// revision unless it was an application or writer write, wakes the
// failover loop when membership changed, and appends its audit record.
// The receipt then carries the state's version numbers and is kept for
// the next entry that reuses the command ID.
func (m *machine) settle(c command, index uint64, r receipt, event *AuditRecord) receipt {
	s := &m.state
	if r.Code == "" {
		if bumpsRevision(c.Kind) {
			s.Revision++
		}
		if changesMembership(c.Kind) {
			m.notifyMembership()
		}
		if event != nil {
			event.CommandID = c.ID
			event.Index = index
			event.Time = c.Time
			event.Actor = c.Actor
			s.Audit = append(s.Audit, *event)
		}
	}
	r.Result.Revision = s.Revision
	r.Result.Index = index
	r.Result.Coordinator = s.Coordinator
	r.Result.AppVersion = s.AppVersion
	r.Result.WriterGeneration = s.WriterGeneration
	m.receipts[c.ID] = r
	r.Result.Data = bytes.Clone(r.Result.Data)
	return r
}

// bumpsRevision says whether an accepted command of this kind versions the
// membership, assignment and policy state; application and writer writes
// are versioned by AppVersion and WriterGeneration instead.
func bumpsRevision(kind string) bool {
	return kind != "app" && kind != "writer"
}

// changesMembership says whether an accepted command of this kind may
// change who is a member or where a member is, which the failover loop
// re-reads.
func changesMembership(kind string) bool {
	switch kind {
	case "initialize", "join_prepare", "join", "remove", "address":
		return true
	}
	return false
}

func applyInitialize(s *State, c command, r *receipt) *AuditRecord {
	if s.Coordinator.Epoch != 0 {
		r.reject("conflict", "cluster is already initialized")
		return nil
	}
	if s.Voters[c.Member.NodeID] != c.Member.Address {
		r.reject("invalid", "initial node is not a voter")
		return nil
	}
	s.Members[c.Member.NodeID] = c.Member
	s.Coordinator = Assignment{NodeID: c.Member.NodeID, Epoch: 1}
	return &AuditRecord{Kind: "coordinator_initialized", To: c.Member.NodeID, Epoch: 1}
}

// applyJoinPrepare admits a member before Raft adds it as a voter. A
// repeated preparation of the same member is accepted as is.
func applyJoinPrepare(s *State, c command, r *receipt) *AuditRecord {
	if c.Member.StorageLevel != "restricted" && c.Member.StorageLevel != "sealed" {
		r.reject("invalid", "full ledger replica requires restricted storage authorization")
		return nil
	}
	if s.Removing[c.Member.NodeID] {
		r.reject("conflict", "member removal is in progress")
		return nil
	}
	if old, ok := s.Members[c.Member.NodeID]; ok {
		if old != c.Member {
			r.reject("conflict", "member already exists with different properties")
		}
		return nil
	}
	// Take the two collisions one map pass at a time. A single pass rejecting
	// on whichever member it happened to reach first would let Go's map
	// iteration order pick the receipt for a newcomer that collides with one
	// member's address and another member's failure domain, and receipts are
	// replicated: they enter the snapshot and answer a retry of the same
	// command ID, so the replicas would disagree about it forever.
	for _, old := range s.Members {
		if old.Address == c.Member.Address {
			r.reject("conflict", "address already belongs to another node")
			return nil
		}
	}
	for _, old := range s.Members {
		if c.Member.FailureDomain != "" && old.FailureDomain == c.Member.FailureDomain {
			r.reject("invalid", "another member already occupies this physical failure domain")
			return nil
		}
	}
	s.Members[c.Member.NodeID] = c.Member
	return nil
}

func applyJoin(s *State, c command, r *receipt) *AuditRecord {
	if s.Voters[c.Member.NodeID] != c.Member.Address {
		r.reject("conflict", "member has not joined the voting configuration")
		return nil
	}
	return &AuditRecord{Kind: "member_joined", To: c.Member.NodeID}
}

func applyRemovePrepare(s *State, c command, r *receipt) *AuditRecord {
	if s.Coordinator.NodeID == c.Remove.NodeID {
		r.reject("conflict", "current coordinator cannot be removed")
		return nil
	}
	if _, ok := s.Members[c.Remove.NodeID]; !ok {
		r.reject("invalid", "member does not exist")
		return nil
	}
	s.Removing[c.Remove.NodeID] = true
	delete(s.PendingAddresses, c.Remove.NodeID)
	return nil
}

func applyRemove(s *State, c command, r *receipt) *AuditRecord {
	if _, ok := s.Voters[c.Remove.NodeID]; ok {
		r.reject("conflict", "member is still in the voting configuration")
		return nil
	}
	if s.Coordinator.NodeID == c.Remove.NodeID {
		r.reject("conflict", "current coordinator cannot be removed")
		return nil
	}
	delete(s.Members, c.Remove.NodeID)
	delete(s.Removing, c.Remove.NodeID)
	delete(s.PendingAddresses, c.Remove.NodeID)
	return &AuditRecord{Kind: "member_removed", From: c.Remove.NodeID}
}

func applyAddressPrepare(s *State, c command, r *receipt) *AuditRecord {
	request := c.Address
	if s.Revision != request.ExpectedRevision {
		r.reject("conflict", "member address revision changed")
		return nil
	}
	if _, ok := s.Members[request.NodeID]; !ok || s.Voters[request.NodeID] == "" || s.Removing[request.NodeID] {
		r.reject("invalid", "address changes require an active voting member")
		return nil
	}
	if pending, ok := s.PendingAddresses[request.NodeID]; ok && pending.ID != request.ID {
		r.reject("conflict", "another address update is pending")
		return nil
	}
	for id, member := range s.Members {
		if id != request.NodeID && (member.Address == request.Address || member.APIAddress == request.APIAddress) {
			r.reject("conflict", "address belongs to another member")
			return nil
		}
	}
	s.PendingAddresses[request.NodeID] = request
	return nil
}

func applyAddress(s *State, c command, r *receipt) *AuditRecord {
	request := c.Address
	if pending, ok := s.PendingAddresses[request.NodeID]; !ok || pending != request {
		r.reject("conflict", "address update preparation differs")
		return nil
	}
	if s.Voters[request.NodeID] != request.Address {
		r.reject("conflict", "voting configuration has not adopted the new address")
		return nil
	}
	member := s.Members[request.NodeID]
	event := &AuditRecord{Kind: "member_address_changed", From: request.NodeID, To: request.NodeID, Reason: fmt.Sprintf("Raft %s -> %s; HTTPS %s -> %s", member.Address, request.Address, member.APIAddress, request.APIAddress)}
	member.Address = request.Address
	member.APIAddress = request.APIAddress
	s.Members[request.NodeID] = member
	delete(s.PendingAddresses, request.NodeID)
	return event
}

func applyPolicy(s *State, c command, r *receipt) *AuditRecord {
	if c.Policy.ExpectedRevision != s.Revision {
		r.reject("conflict", "policy revision changed")
		return nil
	}
	if c.Policy.Enabled && !s.CanAutoFailover() {
		r.reject("invalid", "automatic failover needs at least three voters and two eligible nodes")
		return nil
	}
	s.AutoFailover = c.Policy.Enabled
	event := &AuditRecord{Kind: "automatic_failover_disabled"}
	if c.Policy.Enabled {
		event.Kind = "automatic_failover_enabled"
	}
	return event
}

func applyEligibility(s *State, c command, r *receipt) *AuditRecord {
	if c.Eligibility.ExpectedRevision != s.Revision {
		r.reject("conflict", "membership revision changed")
		return nil
	}
	member, ok := s.Members[c.Eligibility.NodeID]
	if !ok || s.Voters[member.NodeID] == "" {
		r.reject("invalid", "node is not a voting member")
		return nil
	}
	member.AutoEligible = c.Eligibility.Eligible
	s.Members[member.NodeID] = member
	event := &AuditRecord{Kind: "automatic_eligibility_removed", To: member.NodeID}
	if member.AutoEligible {
		event.Kind = "automatic_eligibility_granted"
	}
	return event
}

func applyTransfer(s *State, c command, r *receipt) *AuditRecord {
	if c.Transfer.ExpectedEpoch != s.Coordinator.Epoch {
		r.reject("epoch", "coordinator assignment changed")
		return nil
	}
	if c.ExpectedAppVersion != s.AppVersion || c.ExpectedConfigurationIndex != s.ConfigurationIndex {
		r.reject("conflict", "replicated state changed while preparing transfer")
		return nil
	}
	member, ok := s.Members[c.Transfer.TargetNodeID]
	if !ok || s.Voters[member.NodeID] == "" || s.Removing[member.NodeID] {
		r.reject("invalid", "target is not a voting member")
		return nil
	}
	if c.Automatic && (!s.AutoFailover || !s.CanAutoFailover() || !member.AutoEligible) {
		r.reject("invalid", "automatic transfer is not permitted")
		return nil
	}
	if s.Coordinator.NodeID == member.NodeID {
		r.reject("invalid", "target already coordinates this cluster")
		return nil
	}
	event := &AuditRecord{Kind: "coordinator_transferred", From: s.Coordinator.NodeID, To: member.NodeID, Epoch: s.Coordinator.Epoch + 1, Reason: c.Transfer.Reason}
	s.Coordinator = Assignment{NodeID: member.NodeID, Epoch: s.Coordinator.Epoch + 1}
	return event
}

func applyWriter(s *State, c command, r *receipt) *AuditRecord {
	if c.Writer.CoordinatorEpoch != s.Coordinator.Epoch {
		r.reject("epoch", "coordinator assignment changed")
		return nil
	}
	if c.Writer.CallerNodeID != s.Coordinator.NodeID {
		r.reject("coordinator", "caller does not hold the coordinator role")
		return nil
	}
	if c.Writer.ExpectedGeneration != s.WriterGeneration {
		r.reject("conflict", "writer generation changed")
		return nil
	}
	if s.WriterGeneration == ^uint64(0) {
		r.reject("invalid", "writer generation exhausted")
		return nil
	}
	s.WriterGeneration++
	return nil
}

// applyApp hands a fenced write to the application replica. The
// application's reply is the receipt's data; an application error is a
// local storage failure and stops this replica.
func (m *machine) applyApp(c command, index uint64, r *receipt) error {
	s := &m.state
	if c.App.CoordinatorEpoch != s.Coordinator.Epoch {
		r.reject("epoch", "coordinator assignment changed")
		return nil
	}
	if c.App.CallerNodeID != s.Coordinator.NodeID {
		r.reject("coordinator", "caller does not hold the coordinator role")
		return nil
	}
	if c.App.WriterGeneration == 0 || c.App.WriterGeneration != s.WriterGeneration {
		r.reject("writer", "writer generation changed")
		return nil
	}
	if c.App.ExpectedVersion != s.AppVersion {
		r.reject("conflict", "application version changed")
		return nil
	}
	if m.app == nil {
		return fmt.Errorf("application command received without an application replica")
	}
	data, err := m.app.Apply(AppliedCommand{ID: c.App.ID, RaftIndex: index, Version: s.AppVersion + 1, Payload: bytes.Clone(c.App.Payload)})
	if err != nil {
		return fmt.Errorf("apply index %d: %w", index, err)
	}
	s.AppVersion++
	r.Result.Data = bytes.Clone(data)
	return nil
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
