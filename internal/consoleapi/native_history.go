package consoleapi

import "github.com/gopact-ai/steve/internal/nativehistory"

type NativeImportRequest struct {
	CommandID string               `json:"command_id"`
	Project   string               `json:"project"`
	Agent     string               `json:"agent"`
	Source    nativehistory.Source `json:"source"`
	NativeID  string               `json:"native_id"`
	Revision  string               `json:"revision"`
}

type ImportedSession struct {
	Conversation string                  `json:"conversation"`
	Node         string                  `json:"node"`
	Project      string                  `json:"project"`
	Agent        string                  `json:"agent"`
	Reference    nativehistory.Reference `json:"reference"`
}
