package nodewire

import (
	"context"
	"errors"
)

// Codes a node gives a session command's error, so the hub can tell how a
// remote prompt ended without reading its message.
const (
	SessionErrorDeadline = "deadline_exceeded"
	SessionErrorCanceled = "canceled"
	SessionErrorFailed   = "failed"
)

// SessionErrorCode is the code of a command's run error: empty for none.
func SessionErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return SessionErrorDeadline
	case errors.Is(err, context.Canceled):
		return SessionErrorCanceled
	default:
		return SessionErrorFailed
	}
}
