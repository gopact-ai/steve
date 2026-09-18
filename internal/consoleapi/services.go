package consoleapi

import (
	"context"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

type RestartOperation struct {
	CommandID           string                `json:"command_id,omitempty"`
	State               nodewire.RestartState `json:"state"`
	Incarnation         int64                 `json:"incarnation"`
	PreviousIncarnation int64                 `json:"previous_incarnation,omitempty"`
	RequestedAt         time.Time             `json:"requested_at,omitempty"`
	CompletedAt         time.Time             `json:"completed_at,omitempty"`
	Error               string                `json:"error,omitempty"`
	// Mode is how the request was made: RestartNow or RestartWhenIdle.
	Mode string `json:"mode,omitempty"`
	// WaitingOn names what still keeps a waiting restart from applying,
	// as a stable reason a client can put in the reader's language.
	WaitingOn string `json:"waiting_on,omitempty"`
	// WaitingConversations are the conversations the reason belongs to, so
	// a console can name them the way the reader knows them instead of
	// saying that something is busy somewhere.
	WaitingConversations []string `json:"waiting_conversations,omitempty"`
}
type ManagedService struct {
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Label     string            `json:"label"`
	Online    bool              `json:"online"`
	Version   string            `json:"version"`
	Supported bool              `json:"supported"`
	Operation *RestartOperation `json:"operation,omitempty"`
}
type ServicesView struct {
	Services []ManagedService `json:"services"`
}

// Restart modes. A service that restarts now ends whatever it is doing;
// one that waits keeps the request until the work it would have cut short
// has finished, and then restarts itself on the program now installed.
const (
	RestartNow      = "now"
	RestartWhenIdle = "when-idle"
)

// Reasons a waiting restart reports while it has not applied yet. They
// are stable words a console can put in the reader's language.
const (
	RestartWaitPreparing     = "preparing"
	RestartWaitRequests      = "requests"
	RestartWaitConversations = "conversations"
	RestartWaitChannel       = "channel"
	// RestartWaitQuestion is a turn parked on a question only the owner can
	// answer. It is reported ahead of the channel it blocks, because the
	// wait ends when they answer it and not on its own.
	RestartWaitQuestion   = "question"
	RestartWaitExecutions = "executions"
	RestartWaitCopy       = "copy"
	RestartWaitAttempts   = "attempts"
	RestartWaitAgents     = "agents"
	RestartWaitNode       = "node"
	RestartWaitOffline    = "offline"
)

type RestartRequest struct {
	CommandID string `json:"command_id"`
	// Mode defaults to RestartNow.
	Mode string `json:"mode,omitempty"`
	// Cancel withdraws a waiting restart that has not applied yet.
	Cancel bool `json:"cancel,omitempty"`
	// Program names the build the service must be running afterwards. A
	// launcher that replaced the installed application sets it, because
	// rebuilding the service in place would keep the old program running
	// and report an upgrade that never happened. Left empty the service
	// restarts on the program it already runs.
	Program string `json:"program,omitempty"`
}
type ServiceError struct {
	Code    string
	Message string
	// Reason names what the service is busy with, so a waiting restart can
	// report it as something a reader recognizes rather than as a failure.
	Reason string
	// Subjects are the conversations the reason belongs to.
	Subjects []string
}

func (e *ServiceError) Error() string { return e.Message }

type ServiceControl interface {
	Services(context.Context) (ServicesView, error)
	Restart(context.Context, string, RestartRequest) (RestartOperation, error)
	RestartStatus(context.Context, string, string) (RestartOperation, error)
	RestartAccepted(string, string)
}
