package attempt

import (
	"context"
	"errors"
	"time"
)

type ForceStop struct {
	Revision        uint64    `json:"revision"`
	RequestedAt     time.Time `json:"requested_at"`
	By              string    `json:"by"`
	Level           string    `json:"level"`
	LevelSince      time.Time `json:"level_since"`
	UnansweredSince time.Time `json:"unanswered_since,omitempty"`
	UnansweredCount uint8     `json:"unanswered_count,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	ExhaustedAt     time.Time `json:"exhausted_at,omitempty"`
}

var ErrForceStopChanged = errors.New("force stop request changed")

func (s *Service) RequestForceStop(context.Context, string, string) (Record, error) {
	return Record{}, errors.New("force stop unavailable")
}
func (s *Service) RecordForceStopResult(context.Context, string, uint64, bool, string) (Record, error) {
	return Record{}, errors.New("force stop unavailable")
}
func (s *Service) ConfirmForceStopped(context.Context, string, uint64, RetainedEvidence) (Record, error) {
	return Record{}, errors.New("force stop unavailable")
}
