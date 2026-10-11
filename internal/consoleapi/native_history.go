package consoleapi

import (
	"context"
	"github.com/gopact-ai/steve/internal/conversationapi"

	"github.com/gopact-ai/steve/internal/nativehistory"
)

// NativeHistoryService lists the sessions a machine's coding tools recorded
// on their own, and imports one of them as a console conversation.
type NativeHistoryService interface {
	NativeHistory(context.Context, string, nativehistory.Source) ([]nativehistory.Entry, error)
	ImportNativeHistory(context.Context, string, NativeImportRequest) (ImportedSession, error)
}

type NativeImportRequest = conversationapi.NativeImportRequest

type ImportedSession = conversationapi.ImportedSession
