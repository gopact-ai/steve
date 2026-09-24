// Package coordination maintains cluster membership and the fenced coordinator
// assignment in a replicated Raft state machine.
package coordination

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hashicorp/raft"
)

var (
	ErrNotLeader       = errors.New("coordination: not consensus leader")
	ErrConflict        = errors.New("coordination: state changed")
	ErrStaleEpoch      = errors.New("coordination: stale coordinator epoch")
	ErrStaleWriter     = errors.New("coordination: stale writer generation")
	ErrNotCoordinator  = errors.New("coordination: caller is not coordinator")
	ErrUnavailable     = errors.New("coordination: unavailable")
	ErrNotReady        = errors.New("coordination: target has not caught up")
	ErrInvalid         = errors.New("coordination: invalid request")
	ErrCommandConflict = errors.New("coordination: command ID reused with different input")
	ErrApplication     = errors.New("coordination: application replica failed")
	ErrReceiptExpired  = errors.New("coordination: application replay receipt expired")
)

// ControlProtocolVersion covers prepared voting changes and nonvoter
// coordinators. Missing capability fields from older binaries mean version 0.
const ControlProtocolVersion uint64 = 1

// Application applies deterministic atomic changes. A durable implementation
// must store (Version, ID) and its result in the same transaction as the change: committed
// Raft entries can be replayed after a process restart. Returning an error is a
// fatal local storage failure and stops the replica. Business rejections belong
// in the returned bytes. Snapshot and Restore include that durable deduplication
// state. Snapshot returns owned bytes that remain immutable after Apply resumes.
// No callback may call back into Service.
type Application interface {
	Apply(AppliedCommand) ([]byte, error)
	Snapshot() ([]byte, error)
	Restore([]byte) error
}

// CheckpointApplication takes a snapshot in two steps, so that producing its
// bytes does not hold Apply back. SnapshotCheckpoint runs with Apply excluded
// and only fixes the state the snapshot holds; the Checkpoint it returns is
// encoded while commands apply again. The encoding may compact physical
// replay evidence at or below replayFloor. No Checkpoint method may call
// Service.
type CheckpointApplication interface {
	Application
	SnapshotCheckpoint(replayFloor uint64) (Checkpoint, error)
}

// Checkpoint is an application's state fixed at a snapshot's boundary.
type Checkpoint interface {
	// Encode returns the state as of the boundary. It is called at most
	// once, concurrently with Apply.
	Encode() ([]byte, error)
	// Persisted runs only after the consensus snapshot holding the encoding
	// is durable, and never after another Restore. Failing that optional
	// cleanup must leave live application facts unchanged.
	Persisted() error
	// Release lets the boundary go, encoded or not.
	Release()
}

type AppliedCommand struct {
	ID        string `json:"id"`
	RaftIndex uint64 `json:"raft_index"`
	Version   uint64 `json:"version"`
	Payload   []byte `json:"payload"`
}

type AppCommand struct {
	ID               string `json:"id"`
	CallerNodeID     string `json:"caller_node_id"`
	CoordinatorEpoch uint64 `json:"coordinator_epoch"`
	// Retries retain this version and the original ID. Once it precedes
	// AppReplayFloor the caller must reconcile business facts, not submit
	// the old mutation with a newer version. Application command identity is
	// (ExpectedVersion, ID); administrative command identities are separate.
	ExpectedVersion  uint64 `json:"expected_version"`
	WriterGeneration uint64 `json:"writer_generation"`
	Payload          []byte `json:"payload"`
}

type WriterRequest struct {
	ID                 string `json:"id"`
	CallerNodeID       string `json:"caller_node_id"`
	CoordinatorEpoch   uint64 `json:"coordinator_epoch"`
	ExpectedGeneration uint64 `json:"expected_generation"`
}

type Member struct {
	NodeID        string `json:"node_id"`
	Address       string `json:"address"`
	APIAddress    string `json:"api_address,omitempty"`
	Name          string `json:"name,omitempty"`
	AutoEligible  bool   `json:"auto_eligible"`
	FailureDomain string `json:"failure_domain"`
	StorageLevel  string `json:"storage_level"`
	// Voting says whether this member should hold a Raft vote. A member
	// that only replicates the ledger keeps quorum at the nodes that can
	// answer instantly, so a slow or tunnelled link cannot cost the
	// cluster its leader. Members recorded before this field existed decode
	// as non-voting and the coordinator demotes them, except itself.
	Voting bool `json:"voting"`
}

// MemberNameLimit bounds a display name in characters, not bytes, so
// names in any script get the same room.
const MemberNameLimit = 64

// MemberName trims a requested display name and rejects blank, oversized
// or control-character names with ErrInvalid.
func MemberName(requested string) (string, error) {
	name := strings.TrimSpace(requested)
	if name == "" {
		return "", fmt.Errorf("%w: display name is required", ErrInvalid)
	}
	if utf8.RuneCountInString(name) > MemberNameLimit {
		return "", fmt.Errorf("%w: display name exceeds %d characters", ErrInvalid, MemberNameLimit)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: display name contains control characters", ErrInvalid)
		}
	}
	return name, nil
}

type Assignment struct {
	NodeID string `json:"node_id"`
	Epoch  uint64 `json:"epoch"`
}

type AuditRecord struct {
	CommandID string    `json:"command_id"`
	Index     uint64    `json:"index"`
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	Actor     string    `json:"actor"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Epoch     uint64    `json:"epoch,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

type State struct {
	ClusterID string `json:"cluster_id"`
	// Revision versions membership, coordinator assignment and policy only.
	// Application writes use AppVersion and WriterGeneration instead.
	Revision           uint64            `json:"revision"`
	AppliedIndex       uint64            `json:"applied_index"`
	ConfigurationIndex uint64            `json:"configuration_index"`
	Members            map[string]Member `json:"members"`
	// Replicas holds every server in the Raft configuration, voting or not,
	// while Voters holds only those with a vote.
	Replicas         map[string]string               `json:"replicas"`
	Voters           map[string]string               `json:"voters"`
	Removing         map[string]bool                 `json:"removing"`
	PendingAddresses map[string]MemberAddressRequest `json:"pending_addresses"`
	PendingJoins     map[string]bool                 `json:"pending_joins"`
	PendingVotes     map[string]VotingRequest        `json:"pending_votes"`
	// RequiredControlProtocol is raised when new control semantics first commit.
	// Downgrades after activation are unsupported: old binaries cannot enforce
	// this floor or preserve the additive snapshot fields.
	RequiredControlProtocol uint64        `json:"required_control_protocol,omitempty"`
	Coordinator             Assignment    `json:"coordinator"`
	AutoFailover            bool          `json:"auto_failover"`
	AppVersion              uint64        `json:"app_version"`
	AppReplayFloor          uint64        `json:"app_replay_floor"`
	WriterGeneration        uint64        `json:"writer_generation"`
	Audit                   []AuditRecord `json:"audit"`
}

// IsActiveReplica excludes incomplete joins and removals from business authority.
// Voting is a separate consensus role, not a prerequisite for manual assignment.
func (s State) IsActiveReplica(nodeID string) bool {
	member, ok := s.Members[nodeID]
	return ok && member.NodeID == nodeID && member.Address != "" && s.Replicas[nodeID] == member.Address && !s.PendingJoins[nodeID] && !s.Removing[nodeID]
}

func (s State) CanAutoFailover() bool {
	if len(s.Voters) < 3 {
		return false
	}
	eligible := 0
	domains := map[string]bool{}
	for id := range s.Voters {
		domain := s.Members[id].FailureDomain
		if domain == "" || domains[domain] {
			return false
		}
		domains[domain] = true
		if s.Members[id].AutoEligible && !s.Removing[id] {
			eligible++
		}
	}
	return eligible >= 2
}

type Progress struct {
	ControlProtocol uint64 `json:"control_protocol"`
	ClusterID       string `json:"cluster_id"`
	NodeID          string `json:"node_id"`
	AppliedIndex    uint64 `json:"applied_index"`
	AppVersion      uint64 `json:"app_version"`
	FailureDomain   string `json:"failure_domain"`
	StorageLevel    string `json:"storage_level"`
}

type Status struct {
	State
	ControlProtocol uint64 `json:"control_protocol"`
	NodeID          string `json:"node_id"`
	Address         string `json:"address"`
	LeaderID        string `json:"leader_id"`
	LeaderAddress   string `json:"leader_address"`
	IsLeader        bool   `json:"is_leader"`
	// Build is the program build this node runs, as it names itself.
	Build         string `json:"build,omitempty"`
	Healthy       bool   `json:"healthy"`
	FailureDomain string `json:"failure_domain"`
	StorageLevel  string `json:"storage_level"`
}

func (s Status) Progress() Progress {
	return Progress{ControlProtocol: s.ControlProtocol, ClusterID: s.ClusterID, NodeID: s.NodeID, AppliedIndex: s.AppliedIndex, AppVersion: s.AppVersion, FailureDomain: s.FailureDomain, StorageLevel: s.StorageLevel}
}

type Result struct {
	Revision         uint64     `json:"revision"`
	Index            uint64     `json:"index"`
	Coordinator      Assignment `json:"coordinator"`
	AppVersion       uint64     `json:"app_version"`
	WriterGeneration uint64     `json:"writer_generation"`
	Data             []byte     `json:"data,omitempty"`
}

type TransferRequest struct {
	ID            string `json:"id"`
	Actor         string `json:"actor"`
	ExpectedEpoch uint64 `json:"expected_epoch"`
	TargetNodeID  string `json:"target_node_id"`
	Reason        string `json:"reason"`
}

type PolicyRequest struct {
	ID               string `json:"id"`
	Actor            string `json:"actor"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Enabled          bool   `json:"enabled"`
}

type EligibilityRequest struct {
	ID               string `json:"id"`
	Actor            string `json:"actor"`
	ExpectedRevision uint64 `json:"expected_revision"`
	NodeID           string `json:"node_id"`
	Eligible         bool   `json:"eligible"`
}

type JoinRequest struct {
	ID     string `json:"id"`
	Actor  string `json:"actor"`
	Member Member `json:"member"`
}

// VotingRequest grants or revokes a member's Raft vote. Machines join
// without one so that quorum stays with the nodes that answer instantly;
// a machine has to be promoted before it can take the coordinator role.
type VotingRequest struct {
	ID               string `json:"id"`
	Actor            string `json:"actor"`
	ExpectedRevision uint64 `json:"expected_revision"`
	NodeID           string `json:"node_id"`
	Voting           bool   `json:"voting"`
}

type RemoveRequest struct {
	ID     string `json:"id"`
	Actor  string `json:"actor"`
	NodeID string `json:"node_id"`
}

// RenameRequest changes the name people see for a member. The node ID it
// names stays the identity every task, project and agent refers to.
type RenameRequest struct {
	ID               string `json:"id"`
	Actor            string `json:"actor"`
	ExpectedRevision uint64 `json:"expected_revision"`
	NodeID           string `json:"node_id"`
	Name             string `json:"name"`
}

type MemberAddressRequest struct {
	ID               string `json:"id"`
	Actor            string `json:"actor"`
	ExpectedRevision uint64 `json:"expected_revision"`
	NodeID           string `json:"node_id"`
	Address          string `json:"address"`
	APIAddress       string `json:"api_address"`
}

type Config struct {
	ClusterID        string
	NodeID           string
	FailureDomain    string
	StorageLevel     string
	DataDir          string
	BindAddress      string
	AdvertiseAddress string
	APIAddress       string
	Name             string
	// Build is the program build this node runs; peers read it from Status
	// to tell which program a machine came back on.
	Build       string
	Bootstrap   bool
	Application Application
	// Probe must authenticate the remote node and report its actual FSM progress.
	Probe func(context.Context, Member) (Progress, error)
	// ValidateJoin runs after the candidate has caught up as a nonvoter and
	// before it can vote. A failure leaves a retryable, nonvoting member.
	ValidateJoin func(context.Context, Member) error
	// ValidateAddress verifies all current peers can use a proposed endpoint
	// after its preparation is committed but before voting addresses change.
	ValidateAddress func(context.Context, Member) error
	// AuthorizeReplica runs before any candidate metadata or application bytes
	// are replicated. A full replica must be authorized for the whole ledger.
	AuthorizeReplica func(context.Context, Member) error
	// StreamLayer must authenticate peers for non-loopback networking. Without
	// it, Open accepts only a loopback bind and loopback advertised address.
	StreamLayer     raft.StreamLayer
	LogOutput       io.Writer
	ApplyTimeout    time.Duration
	FailoverTimeout time.Duration
	ProbeInterval   time.Duration
	// RaftConfig optionally changes Raft timings for embedded deployments/tests.
	// LocalID and LogOutput are always supplied by Open.
	RaftConfig *raft.Config
}
