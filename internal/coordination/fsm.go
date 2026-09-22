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
	Rename                     RenameRequest        `json:"rename,omitempty"`
	Voting                     VotingRequest        `json:"voting,omitempty"`
	Remove                     RemoveRequest        `json:"remove,omitempty"`
	Address                    MemberAddressRequest `json:"address,omitempty"`
	App                        AppCommand           `json:"app,omitempty"`
	Writer                     WriterRequest        `json:"writer,omitempty"`
	Automatic                  bool                 `json:"automatic,omitempty"`
	ExpectedAppVersion         uint64               `json:"expected_app_version,omitempty"`
	ExpectedConfigurationIndex uint64               `json:"expected_configuration_index,omitempty"`
	MemberControlProtocol      uint64               `json:"member_control_protocol,omitempty"`
}

type receipt struct {
	Fingerprint        string  `json:"fingerprint"`
	Result             Result  `json:"result"`
	Code               string  `json:"code,omitempty"`
	Message            string  `json:"message,omitempty"`
	ApplicationVersion *uint64 `json:"application_version,omitempty"`
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
	case "expired":
		kind = ErrReceiptExpired
	default:
		kind = ErrInvalid
	}
	return fmt.Errorf("%w: %s", kind, r.Message)
}

type machine struct {
	mu                 sync.RWMutex
	state              State
	receipts           map[string]receipt
	app                Application
	failure            error
	failed             chan struct{}
	membershipChanged  chan struct{}
	snapshotGeneration uint64
}

func newMachine(clusterID string, app Application) *machine {
	return &machine{state: State{ClusterID: clusterID, Members: map[string]Member{}, Replicas: map[string]string{}, Voters: map[string]string{}, Removing: map[string]bool{}, PendingAddresses: map[string]MemberAddressRequest{}, PendingJoins: map[string]bool{}, PendingVotes: map[string]VotingRequest{}}, receipts: map[string]receipt{}, app: app, failed: make(chan struct{}), membershipChanged: make(chan struct{}, 1)}
}

func (m *machine) read() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneState(m.state)
}

// memberNames copies only the display names, so callers that render a
// snapshot do not clone the audit log on every read.
func (m *machine) memberNames() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make(map[string]string, len(m.state.Members))
	for id, member := range m.state.Members {
		if member.Name != "" {
			names[id] = member.Name
		}
	}
	return names
}

func cloneState(s State) State {
	s.Members = maps.Clone(s.Members)
	s.Replicas = maps.Clone(s.Replicas)
	s.Voters = maps.Clone(s.Voters)
	s.Removing = maps.Clone(s.Removing)
	s.PendingAddresses = maps.Clone(s.PendingAddresses)
	s.PendingJoins = maps.Clone(s.PendingJoins)
	s.PendingVotes = maps.Clone(s.PendingVotes)
	s.Audit = slices.Clone(s.Audit)
	return s
}

func (m *machine) lookup(id, fingerprint string) (receipt, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.receipts["control/"+id]
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
	if c.Kind == "app" && c.App.ExpectedVersion < m.state.AppReplayFloor {
		return expiredReceipt()
	}
	if old, ok := m.receipts[receiptKey(c)]; ok {
		if old.Fingerprint != c.Fingerprint {
			return receipt{Code: "command", Message: "command ID belongs to different input"}
		}
		old.Result.Data = bytes.Clone(old.Result.Data)
		return old
	}
	r := receipt{Fingerprint: c.Fingerprint}
	previousControlProtocol := m.state.RequiredControlProtocol
	event, err := m.applyCommand(c, log.Index, &r)
	if err != nil {
		return m.fail(err)
	}
	return m.settle(c, log.Index, r, event, previousControlProtocol)
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
	case "rename":
		return applyRename(s, c, r), nil
	case "voting_prepare":
		return applyVotingPrepare(s, c, r), nil
	case "voting":
		return applyVoting(s, c, r), nil
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
func (m *machine) settle(c command, index uint64, r receipt, event *AuditRecord, previousControlProtocol uint64) receipt {
	s := &m.state
	if c.Kind == "app" {
		version := c.App.ExpectedVersion
		r.ApplicationVersion = &version
	}
	if r.Code == "" {
		if bumpsRevision(c.Kind) {
			s.Revision++
		}
		if changesMembership(c.Kind) {
			m.notifyMembership()
		}
		// Keep a marker in the legacy audit shape when a new control
		// protocol is activated. Older binaries ignore the additive state
		// fields in a format-3 snapshot, but they preserve this known
		// AuditRecord representation. A newer binary can therefore fail
		// closed if an old binary rewrites a snapshot after activation
		// instead of silently treating the activated state as legacy.
		if previousControlProtocol < ControlProtocolVersion && s.RequiredControlProtocol >= ControlProtocolVersion {
			s.Audit = append(s.Audit, AuditRecord{
				CommandID: c.ID + "/control-protocol",
				Index:     index,
				Time:      c.Time,
				Actor:     c.Actor,
				Kind:      "control_protocol_required",
				Reason:    fmt.Sprintf("protocol=%d", s.RequiredControlProtocol),
			})
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
	m.receipts[receiptKey(c)] = r
	m.advanceReplayFloor()
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
	case "initialize", "join_prepare", "join", "remove", "address", "voting_prepare", "voting":
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
	if c.MemberControlProtocol < s.RequiredControlProtocol {
		r.reject("invalid", "joining node control protocol is too old; downgrade is unsupported")
		return nil
	}
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
	if s.PendingJoins == nil {
		s.PendingJoins = map[string]bool{}
	}
	s.PendingJoins[c.Member.NodeID] = true
	return nil
}

func applyJoin(s *State, c command, r *receipt) *AuditRecord {
	if s.Replicas[c.Member.NodeID] != c.Member.Address {
		r.reject("conflict", "member has not joined the replication configuration")
		return nil
	}
	if c.Member.Voting && s.Voters[c.Member.NodeID] != c.Member.Address {
		r.reject("conflict", "member has not joined the voting configuration")
		return nil
	}
	delete(s.PendingJoins, c.Member.NodeID)
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
	if _, pending := s.PendingVotes[c.Remove.NodeID]; pending {
		r.reject("conflict", "voting change is pending")
		return nil
	}
	s.Removing[c.Remove.NodeID] = true
	delete(s.PendingAddresses, c.Remove.NodeID)
	return nil
}

func applyRemove(s *State, c command, r *receipt) *AuditRecord {
	if _, ok := s.Replicas[c.Remove.NodeID]; ok {
		r.reject("conflict", "member is still in the replication configuration")
		return nil
	}
	if s.Coordinator.NodeID == c.Remove.NodeID {
		r.reject("conflict", "current coordinator cannot be removed")
		return nil
	}
	delete(s.Members, c.Remove.NodeID)
	delete(s.PendingJoins, c.Remove.NodeID)
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
	if _, ok := s.Members[request.NodeID]; !ok || s.Replicas[request.NodeID] == "" || s.Removing[request.NodeID] {
		r.reject("invalid", "address changes require an active member")
		return nil
	}
	if _, pending := s.PendingVotes[request.NodeID]; pending {
		r.reject("conflict", "voting change is pending")
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
	if s.Replicas[request.NodeID] != request.Address {
		r.reject("conflict", "replication configuration has not adopted the new address")
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
	if c.Policy.Enabled && s.Voters[s.Coordinator.NodeID] == "" {
		r.reject("invalid", "automatic failover requires the current coordinator to be a voter")
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

func applyRename(s *State, c command, r *receipt) *AuditRecord {
	if c.Rename.ExpectedRevision != s.Revision {
		r.reject("conflict", "membership revision changed")
		return nil
	}
	member, ok := s.Members[c.Rename.NodeID]
	if !ok {
		r.reject("invalid", "node is not a member")
		return nil
	}
	name, err := MemberName(c.Rename.Name)
	if err != nil {
		r.reject("invalid", err.Error())
		return nil
	}
	previous := member.Name
	member.Name = name
	s.Members[member.NodeID] = member
	return &AuditRecord{Kind: "member_renamed", To: member.NodeID, Reason: fmt.Sprintf("%s -> %s", previous, name)}
}

// applyVotingPrepare retains the reviewed change across a lost response or
// leadership change between the Raft configuration and its final receipt.
func applyVotingPrepare(s *State, c command, r *receipt) *AuditRecord {
	request := c.Voting
	if request.ExpectedRevision != s.Revision {
		r.reject("conflict", "membership revision changed")
		return nil
	}
	if !s.IsActiveReplica(request.NodeID) {
		r.reject("invalid", "node is not an active replica")
		return nil
	}
	if _, pending := s.PendingAddresses[request.NodeID]; pending {
		r.reject("conflict", "member address change is pending")
		return nil
	}
	if !request.Voting && s.Coordinator.NodeID == request.NodeID {
		r.reject("invalid", "the coordinator keeps its vote")
		return nil
	}
	if s.PendingVotes == nil {
		s.PendingVotes = map[string]VotingRequest{}
	}
	// A fresh, explicitly reviewed revision may replace an unfinished intent.
	// Older retries retain their preparation receipt but cannot finalize it.
	s.PendingVotes[request.NodeID] = request
	s.RequiredControlProtocol = max(s.RequiredControlProtocol, ControlProtocolVersion)
	return nil
}

// applyVoting records whether a member should hold a vote. Raft has already
// changed the configuration when this commits; the flag is what survives a
// restart and what tells the coordinator which members to keep demoted.
func applyVoting(s *State, c command, r *receipt) *AuditRecord {
	request := c.Voting
	// Historical voting log entries predate preparation and have no fence.
	// Newly submitted operations always carry a nonzero configuration index.
	if c.ExpectedConfigurationIndex != 0 {
		if pending, ok := s.PendingVotes[request.NodeID]; !ok || pending != request || c.ExpectedConfigurationIndex != s.ConfigurationIndex {
			r.reject("conflict", "voting change preparation or configuration differs")
			return nil
		}
		if !s.IsActiveReplica(request.NodeID) {
			r.reject("invalid", "node is not an active replica")
			return nil
		}
	}
	member, ok := s.Members[request.NodeID]
	if !ok || s.Removing[request.NodeID] {
		r.reject("invalid", "node is not an active member")
		return nil
	}
	if request.Voting && s.Voters[request.NodeID] != member.Address {
		r.reject("conflict", "member has not joined the voting configuration")
		return nil
	}
	if !request.Voting {
		if s.Coordinator.NodeID == request.NodeID {
			r.reject("invalid", "the coordinator keeps its vote")
			return nil
		}
		if s.Voters[request.NodeID] != "" {
			r.reject("conflict", "member is still in the voting configuration")
			return nil
		}
	}
	member.Voting = request.Voting
	s.Members[request.NodeID] = member
	delete(s.PendingVotes, request.NodeID)
	event := &AuditRecord{Kind: "member_vote_revoked", To: request.NodeID}
	if request.Voting {
		event.Kind = "member_vote_granted"
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
	if !ok || !s.IsActiveReplica(member.NodeID) {
		r.reject("invalid", "target is not an active replica")
		return nil
	}
	if _, pending := s.PendingVotes[member.NodeID]; pending {
		r.reject("conflict", "target voting change is pending")
		return nil
	}
	if (c.Automatic || s.AutoFailover) && s.Voters[member.NodeID] != member.Address {
		r.reject("invalid", "automatic policy requires a voting coordinator")
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
	if s.Voters[member.NodeID] != member.Address {
		s.RequiredControlProtocol = max(s.RequiredControlProtocol, ControlProtocolVersion)
	}
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
	m.state.Replicas = make(map[string]string, len(c.Servers))
	m.state.Voters = make(map[string]string, len(c.Servers))
	for _, server := range c.Servers {
		m.state.Replicas[string(server.ID)] = string(server.Address)
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
}

// Keep the existing envelope version during rolling upgrades. New state fields
// are additive; new log semantics are gated separately before submission.
const snapshotFormat = 3

func (m *machine) Snapshot() (raft.FSMSnapshot, error) {
	data, application, persisted, err := m.captureSnapshot()
	if err != nil {
		return nil, err
	}
	// Metadata is an owned copy. JSON work no longer blocks Apply, and the
	// application's binary snapshot never passes through the JSON encoder.
	metadata, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &encodedSnapshot{metadata: metadata, application: application, persisted: persisted}, nil
}

func (m *machine) captureSnapshot() (snapshotData, []byte, func() error, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.failure != nil {
		return snapshotData{}, nil, nil, fmt.Errorf("%w: replica stopped", ErrApplication)
	}
	data := snapshotData{Format: snapshotFormat, State: cloneState(m.state), Receipts: maps.Clone(m.receipts), HasApplication: m.app != nil}
	// Result bytes may be owned by an application implementation. Do not
	// retain aliases while snapshot persistence runs concurrently with Apply.
	for id, receipt := range data.Receipts {
		receipt.Result.Data = bytes.Clone(receipt.Result.Data)
		data.Receipts[id] = receipt
	}
	var application []byte
	var persisted func() error
	if m.app != nil {
		var err error
		if owner, ok := m.app.(CheckpointApplication); ok {
			application, persisted, err = owner.SnapshotCheckpoint(data.State.AppReplayFloor)
		} else {
			application, err = m.app.Snapshot()
		}
		if err != nil {
			return snapshotData{}, nil, nil, fmt.Errorf("snapshot application: %w", err)
		}
	}
	if persisted != nil {
		prune, generation := persisted, m.snapshotGeneration
		persisted = func() error {
			m.mu.RLock()
			defer m.mu.RUnlock()
			if generation != m.snapshotGeneration {
				return nil
			}
			return prune()
		}
	}
	return data, application, persisted, nil
}

func (m *machine) Restore(reader io.ReadCloser) error {
	defer reader.Close()
	metadata, application, err := decodeSnapshot(reader)
	if err != nil {
		return err
	}
	var data snapshotData
	if err := json.Unmarshal(metadata, &data); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if data.Format != snapshotFormat || data.State.ClusterID != m.state.ClusterID || data.State.Members == nil || data.State.Replicas == nil || data.State.Voters == nil || data.State.Removing == nil || data.State.PendingAddresses == nil || data.Receipts == nil {
		return fmt.Errorf("%w: snapshot identity or format differs", ErrInvalid)
	}
	if data.State.RequiredControlProtocol > ControlProtocolVersion {
		return fmt.Errorf("%w: snapshot requires a newer control protocol; downgrade is unsupported", ErrInvalid)
	}
	var fields struct {
		State struct {
			PendingJoins json.RawMessage `json:"pending_joins"`
			PendingVotes json.RawMessage `json:"pending_votes"`
		} `json:"state"`
	}
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return fmt.Errorf("%w: decode snapshot membership state: %v", ErrInvalid, err)
	}
	controlProtocolMarker := false
	for _, event := range data.State.Audit {
		if event.Kind == "control_protocol_required" {
			controlProtocolMarker = true
			break
		}
	}
	if controlProtocolMarker && data.State.RequiredControlProtocol < ControlProtocolVersion {
		return fmt.Errorf("%w: activated control protocol state was lost; downgrade is unsupported", ErrInvalid)
	}
	if data.Format == 3 && len(fields.State.PendingJoins) == 0 && len(fields.State.PendingVotes) == 0 && data.State.RequiredControlProtocol == 0 {
		// Only genuinely legacy snapshots omit BOTH fields. Administrative
		// audit is retained and distinguishes completed joins from admission,
		// including a member removed and then admitted again under the same ID.
		completed := map[string]bool{}
		for _, event := range data.State.Audit {
			switch event.Kind {
			case "coordinator_initialized", "member_joined":
				completed[event.To] = true
			case "member_removed":
				delete(completed, event.From)
			}
		}
		data.State.PendingJoins = map[string]bool{}
		data.State.PendingVotes = map[string]VotingRequest{}
		for id := range data.State.Members {
			if !completed[id] {
				data.State.PendingJoins[id] = true
			}
		}
	} else if data.State.PendingJoins == nil || data.State.PendingVotes == nil {
		return fmt.Errorf("%w: snapshot is missing pending membership state", ErrInvalid)
	}
	if data.HasApplication != (m.app != nil) || (!data.HasApplication && len(application) != 0) {
		return fmt.Errorf("%w: snapshot application configuration differs", ErrInvalid)
	}
	if data.State.AppReplayFloor != applicationReplayFloor(data.State.AppVersion) {
		return fmt.Errorf("%w: snapshot replay window differs", ErrInvalid)
	}
	m.snapshotGeneration++
	if m.app != nil {
		if err := m.app.Restore(application); err != nil {
			m.fail(fmt.Errorf("restore application: %w", err))
			return fmt.Errorf("restore application: %w", err)
		}
	}
	m.state = data.State
	m.receipts = data.Receipts
	m.notifyMembership()
	return nil
}
