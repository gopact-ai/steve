package cluster

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

// joinWindow is how long a fixture keeps retrying a Join the coordinator
// refused as unavailable before it fails the test.
const joinWindow = time.Minute

// joinRetrying joins request.Member through source the way the
// coordination client does: a Join refused as unavailable is retried under
// the same command ID, which the leader deduplicates, until joinWindow
// ends. Join waits for a snapshot of the application baseline only up to
// ApplyTimeout and says it "may still finish later"; under CPU contention
// (the race detector, parallel packages) that bound passes with nothing
// wrong, and a retry shares the snapshot already running instead of
// starting another. Any other error, or the window ending, fails the test
// with every attempt's error and duration.
func joinRetrying(t *testing.T, source *Peer, request coordination.JoinRequest) {
	t.Helper()
	deadline := time.Now().Add(joinWindow)
	var attempts []string
	for attempt := 1; ; attempt++ {
		started := time.Now()
		_, err := source.Join(t.Context(), request)
		if err == nil {
			if len(attempts) > 0 {
				t.Logf("join %s succeeded on attempt %d after: %v", request.Member.NodeID, attempt, attempts)
			}
			return
		}
		attempts = append(attempts, time.Since(started).Round(time.Millisecond).String()+": "+err.Error())
		if !errors.Is(err, coordination.ErrUnavailable) || !time.Now().Before(deadline) {
			t.Fatalf("join %s failed after %d attempts: %v", request.Member.NodeID, attempt, attempts)
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("join %s: %v after %v", request.Member.NodeID, t.Context().Err(), attempts)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
