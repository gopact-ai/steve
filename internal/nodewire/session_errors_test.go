package nodewire

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestSessionErrorCodeNamesContextErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("prompt: %w", context.DeadlineExceeded), SessionErrorDeadline},
		{fmt.Errorf("prompt: %w", context.Canceled), SessionErrorCanceled},
		{errors.New("prompt: " + context.DeadlineExceeded.Error()), SessionErrorFailed},
		{errors.New("exit status 1"), SessionErrorFailed},
	} {
		if got := SessionErrorCode(tc.err); got != tc.want {
			t.Errorf("SessionErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A node built before error codes leaves ErrorCode empty; its message is
// all there is to classify.
func TestLegacySessionErrorCodeReadsAMessageWithoutACode(t *testing.T) {
	for _, tc := range []struct {
		command SessionCommand
		want    string
	}{
		{SessionCommand{}, ""},
		{SessionCommand{Error: "prompt: context deadline exceeded"}, SessionErrorDeadline},
		{SessionCommand{Error: "prompt: context canceled"}, SessionErrorCanceled},
		{SessionCommand{Error: "exit status 1"}, SessionErrorFailed},
		{SessionCommand{Error: "context deadline exceeded", ErrorCode: SessionErrorFailed}, SessionErrorFailed},
		{SessionCommand{Error: "stopped", ErrorCode: SessionErrorCanceled}, SessionErrorCanceled},
	} {
		if got := tc.command.ErrorKind(); got != tc.want {
			t.Errorf("%+v.ErrorKind() = %q, want %q", tc.command, got, tc.want)
		}
	}
}
