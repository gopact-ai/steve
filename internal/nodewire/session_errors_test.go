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
