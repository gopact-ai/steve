// Package coordination maintains cluster membership and the fenced coordinator
// assignment in a replicated Raft state machine.
package coordination

import (
	"context"
	"errors"
	"io"
	"time"

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
)

// Application applies deterministic atomic changes. A durable implementation
// must store ID and its result in the same transaction as the change: committed
// Raft entries can be replayed after a process restart. Returning an error is a
// fatal local storage failure and stops the replica. Business rejections belong
// in the returned bytes. Snapshot and Restore include that durable deduplication
// state. No callback may call back into Service.
type Application interface {
	Apply(AppliedCommand) ([]byte, error)
	Snapshot() ([]byte, error)
	Restore([]byte) error
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
	Revision           uint64                          `json:"revision"`
	AppliedIndex       uint64                          `json:"applied_index"`
	ConfigurationIndex uint64                          `json:"configuration_index"`
	Members            map[string]Member               `json:"members"`
	Voters             map[string]string               `json:"voters"`
	Removing           map[string]bool                 `json:"removing"`
	PendingAddresses   map[string]MemberAddressRequest `json:"pending_addresses"`
	Coordinator        Assignment                      `json:"coordinator"`
	AutoFailover       bool                            `json:"auto_failover"`
	AppVersion         uint64                          `json:"app_version"`
	WriterGeneration   uint64                          `json:"writer_generation"`
	Audit              []AuditRecord                   `json:"audit"`
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
	ClusterID     string `json:"cluster_id"`
	NodeID        string `json:"node_id"`
	AppliedIndex  uint64 `json:"applied_index"`
	AppVersion    uint64 `json:"app_version"`
	FailureDomain string `json:"failure_domain"`
	StorageLevel  string `json:"storage_level"`
}

type Status struct {
	State
	NodeID        string `json:"node_id"`
	Address       string `json:"address"`
	LeaderID      string `json:"leader_id"`
	LeaderAddress string `json:"leader_address"`
	IsLeader      bool   `json:"is_leader"`
	Healthy       bool   `json:"healthy"`
	FailureDomain string `json:"failure_domain"`
	StorageLevel  string `json:"storage_level"`
}

func (s Status) Progress() Progress {
	return Progress{ClusterID: s.ClusterID, NodeID: s.NodeID, AppliedIndex: s.AppliedIndex, AppVersion: s.AppVersion, FailureDomain: s.FailureDomain, StorageLevel: s.StorageLevel}
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

type RemoveRequest struct {
	ID     string `json:"id"`
	Actor  string `json:"actor"`
	NodeID string `json:"node_id"`
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
	Bootstrap        bool
	Application      Application
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
