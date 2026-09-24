package consoleapi

import (
	"context"

	"github.com/gopact-ai/steve/internal/nativehistory"
)

// NativeHistoryService lists the sessions a machine's coding tools recorded
// on their own, and imports one of them as a console conversation.
type NativeHistoryService interface {
	NativeHistory(context.Context, string, nativehistory.Source) ([]nativehistory.Entry, error)
	ImportNativeHistory(context.Context, string, NativeImportRequest) (ImportedSession, error)
}

type NativeImportRequest struct {
	CommandID string `json:"command_id"`
	// Project may be empty to associate the original workspace automatically.
	Project  string               `json:"project"`
	Agent    string               `json:"agent"`
	Source   nativehistory.Source `json:"source"`
	NativeID string               `json:"native_id"`
	Revision string               `json:"revision"`
}

type ImportedSession struct {
	Conversation string                  `json:"conversation"`
	Node         string                  `json:"node"`
	Project      string                  `json:"project"`
	Agent        string                  `json:"agent"`
	Reference    nativehistory.Reference `json:"reference"`
}
