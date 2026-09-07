package nodewire

import (
	"errors"
	"time"
)

const StreamRestart = "restart"
const FeatureRestart = "service_restart.v1"

var ErrRestartBusy = errors.New("node is busy; release idle sessions before restarting")
var ErrRestartUnsupported = errors.New("node does not support controlled restart")
var ErrRestartNotFound = errors.New("restart command not found")
var ErrRestartPreflight = errors.New("node restart preflight failed")

type RestartRequest struct {
	Action    string `json:"action"`
	CommandID string `json:"command_id,omitempty"`
}

type RestartStatus struct {
	CommandID           string    `json:"command_id,omitempty"`
	State               string    `json:"state"`
	Supported           bool      `json:"supported"`
	Incarnation         int64     `json:"incarnation"`
	PreviousIncarnation int64     `json:"previous_incarnation,omitempty"`
	RequestedAt         time.Time `json:"requested_at,omitempty"`
	CompletedAt         time.Time `json:"completed_at,omitempty"`
	ActiveStreams       int       `json:"active_streams"`
	Processes           int       `json:"processes"`
	Error               string    `json:"error,omitempty"`
}

type RestartReply struct {
	Status    RestartStatus `json:"status"`
	ErrorCode string        `json:"error_code,omitempty"`
	Error     string        `json:"error,omitempty"`
}
