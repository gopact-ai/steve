package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

// joinWindow is how long a fixture keeps retrying a Join that has not gone
// through yet before it fails the test.
const joinWindow = time.Minute

// joinRetrying joins request.Member through source, retrying a Join that
// has not gone through yet under the same command ID, which the leader
// deduplicates, until joinWindow ends; enrollment polls its join the same
// way. Join waits for its barriers and submit, for a snapshot of the
// application baseline, and for the new member to catch up, each only up
// to ApplyTimeout, and past it says the deadline passed, that it is
// unavailable ("may still finish later") or that the target has not caught
// up. Under CPU contention (the race detector, parallel packages) those
// bounds pass with nothing wrong, and a retry shares the snapshot already
// running instead of starting another. Any other error, or the window
// ending, fails the test with the first and the last attempt's error.
func joinRetrying(t *testing.T, source *Peer, request coordination.JoinRequest) {
	t.Helper()
	started := time.Now()
	deadline := started.Add(joinWindow)
	var first string
	for attempt := 1; ; attempt++ {
		began := time.Now()
		_, err := source.Join(t.Context(), request)
		if err == nil {
			if attempt > 1 {
				t.Logf("join %s succeeded on attempt %d after %s; the first failed after %s", request.Member.NodeID, attempt, time.Since(started).Round(time.Millisecond), first)
			}
			return
		}
		last := time.Since(began).Round(time.Millisecond).String() + ": " + err.Error()
		if first == "" {
			first = last
		}
		retry := errors.Is(err, coordination.ErrUnavailable) || errors.Is(err, coordination.ErrNotReady) || errors.Is(err, context.DeadlineExceeded)
		if !retry || !time.Now().Before(deadline) {
			t.Fatalf("join %s failed after %d attempts over %s; first after %s; last after %s", request.Member.NodeID, attempt, time.Since(started).Round(time.Millisecond), first, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
