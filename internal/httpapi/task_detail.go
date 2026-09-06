package httpapi

import (
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// TaskDetail joins a system projection with the attempts exposed by the application.
type TaskDetail struct {
	Task     readmodel.Task           `json:"task"`
	Plan     *readmodel.Plan          `json:"plan,omitempty"`
	Children []readmodel.Task         `json:"children"`
	Attempts []consoleapi.AttemptView `json:"attempts"`
}
