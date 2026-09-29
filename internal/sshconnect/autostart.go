package sshconnect

import (
	"context"
	"time"
)

type AutoStartBackend interface {
	RestartBackend
	Watched(ctx context.Context) []string
	Answers(ctx context.Context, nodeID string) bool
	Reachable(ctx context.Context, nodeID string) bool
}

const (
	autoStartEvery   = 15 * time.Second
	autoStartAfter   = time.Minute
	autoStartLimit   = 5
	autoStartSettle  = 10 * time.Minute
	autoStartBackoff = time.Minute
	autoStartRecheck = 5 * time.Minute
)

type AutoStartState struct {
	State        string    `json:"state"`
	Attempts     int       `json:"attempts"`
	Limit        int       `json:"limit"`
	LastError    string    `json:"last_error,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	LastAt       time.Time `json:"last_at,omitzero"`
	NextAt       time.Time `json:"next_at,omitzero"`
	OfflineSince time.Time `json:"offline_since,omitzero"`
}

func (s *Service) AutoStart() {}

func (s *Service) sweep() {}
