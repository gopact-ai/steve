package coordination

import (
	"errors"
	"testing"

	"github.com/hashicorp/raft"
)

type resolvedFuture struct{ err error }

func (f resolvedFuture) Error() error { return f.err }

// While its own leadership transfer runs, a leader rejects applies, barriers,
// configuration reads and changes, restores and further transfers. The
// rejection ends with the transfer, so callers must see it as retryable, not
// as a bad request.
func TestWaitReportsTransientRaftRejectionsAsUnavailable(t *testing.T) {
	s := &Service{ctx: t.Context(), fsm: newMachine("test-cluster", nil)}
	for _, cause := range []error{raft.ErrLeadershipTransferInProgress, raft.ErrLeadershipLost, raft.ErrRaftShutdown, raft.ErrEnqueueTimeout} {
		if err := s.wait(t.Context(), resolvedFuture{cause}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%v was reported as %v", cause, err)
		}
	}
}
