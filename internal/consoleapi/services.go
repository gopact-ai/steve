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
type RestartRequest struct {
	CommandID string `json:"command_id"`
}
type ServiceError struct {
	Code    string
	Message string
}

func (e *ServiceError) Error() string { return e.Message }

type ServiceControl interface {
	Services(context.Context) (ServicesView, error)
	Restart(context.Context, string, RestartRequest) (RestartOperation, error)
	RestartStatus(context.Context, string, string) (RestartOperation, error)
	RestartAccepted(string, string)
}
