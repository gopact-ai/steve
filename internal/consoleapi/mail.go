package consoleapi

import (
	"context"
	"encoding/json"
)

// MailControl is a local provider administration port. Credentials bypass the
// conversation, client-invocation ledger and model context entirely.
type MailControl interface {
	Exchange(context.Context, string, string, json.RawMessage) (json.RawMessage, int, error)
}
