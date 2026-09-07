package consoleapi

import (
	"context"
	"time"
)

type CoordinatorNode struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Local        bool   `json:"local"`
	Online       bool   `json:"online"`
	Voter        bool   `json:"voter"`
	AutoEligible bool   `json:"auto_eligible"`
	Ready        bool   `json:"ready"`
	Reason       string `json:"reason,omitempty"`
}

type CoordinatorEvent struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Actor  string    `json:"actor"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

type CoordinationView struct {
	Enabled       bool               `json:"enabled"`
	ClusterID     string             `json:"cluster_id,omitempty"`
	NodeID        string             `json:"node_id,omitempty"`
	CoordinatorID string             `json:"coordinator_id,omitempty"`
	Epoch         uint64             `json:"epoch"`
	Revision      uint64             `json:"revision"`
	Authoritative bool               `json:"authoritative"`
	ObservedAt    time.Time          `json:"observed_at"`
	AutoFailover  bool               `json:"auto_failover"`
	Ready         bool               `json:"ready"`
	Reason        string             `json:"reason,omitempty"`
	Nodes         []CoordinatorNode  `json:"nodes"`
	Events        []CoordinatorEvent `json:"events"`
}

type CoordinatorTransfer struct {
	CommandID     string `json:"command_id"`
	ExpectedEpoch uint64 `json:"expected_epoch"`
	TargetNodeID  string `json:"target_node_id"`
}

type CoordinatorPolicy struct {
	CommandID        string `json:"command_id"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Enabled          bool   `json:"enabled"`
}

type CoordinatorEligibility struct {
	CommandID        string `json:"command_id"`
	ExpectedRevision uint64 `json:"expected_revision"`
	NodeID           string `json:"node_id"`
	Eligible         bool   `json:"eligible"`
}

type CoordinationService interface {
	Coordination(context.Context) (CoordinationView, error)
	TransferCoordinator(context.Context, CoordinatorTransfer) (CoordinationView, error)
	SetAutoFailover(context.Context, CoordinatorPolicy) (CoordinationView, error)
	SetCoordinatorEligibility(context.Context, CoordinatorEligibility) (CoordinationView, error)
}
