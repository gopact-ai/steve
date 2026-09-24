package nodewire

import (
	"context"
	"errors"
	"strings"
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

// ErrorKind is the code of the command's error. A node that predates
// ErrorCode reports only the message, which carried a context error's text;
// that message is classified here and nowhere else.
func (c SessionCommand) ErrorKind() string {
	switch {
	case c.ErrorCode != "":
		return c.ErrorCode
	case c.Error == "":
		return ""
	case strings.Contains(c.Error, context.DeadlineExceeded.Error()):
		return SessionErrorDeadline
	case strings.Contains(c.Error, context.Canceled.Error()):
		return SessionErrorCanceled
	default:
		return SessionErrorFailed
	}
}
