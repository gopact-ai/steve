package sshconnect

import (
	"context"
	"errors"
	"time"
)

// RestartBackend is offered by a backend whose enrolled machines can have
// their peer restarted over the SSH alias they were enrolled through.
type RestartBackend interface {
	RestartTarget(ctx context.Context, nodeID string) (string, error)
	Knows(ctx context.Context, nodeID string) bool
	Restarted(ctx context.Context, nodeID string) error
	RecordRestart(ctx context.Context, record RestartRecord) error
}

const (
	RestartRestarted = "restarted"
	RestartStarted   = "started"
	RestartRunning   = "running"
	RestartFailed    = "failed"
	RestartStopped   = "stopped"
)

type RestartRecord struct {
	NodeID    string    `json:"node_id"`
	By        string    `json:"by,omitempty"`
	Automatic bool      `json:"automatic"`
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

type RestartState struct {
	Restart   *InstallResult  `json:"restart,omitempty"`
	Automatic bool            `json:"automatic,omitempty"`
	AutoStart *AutoStartState `json:"auto_start,omitempty"`
}

func (s *Service) Restart(ctx context.Context, nodeID string) (InstallResult, error) {
	return InstallResult{}, errors.New("restart is not implemented")
}

func (s *Service) RestartStatus(ctx context.Context, nodeID string) (RestartState, error) {
	return RestartState{}, nil
}
