package artifact

import (
	"context"
	"testing"
	"time"
)

func TestRecoveryApplyIgnoresSourceCancellationButStopsOnEpisodeDriverLoss(t *testing.T) {
	episode, loseEpisode := context.WithCancel(t.Context())
	defer loseEpisode()
	source, cancelSource := context.WithCancel(episode)
	ctx := context.WithValue(source, recoveryLandingKey{}, &recoveryLandingPermit{lifetime: episode})
	apply, done := landingApplyContext(ctx)
	defer done()
	cancelSource()
	select {
	case <-apply.Done():
		t.Fatal("source stop interrupted its already admitted recovery WAL")
	default:
	}
	loseEpisode()
	select {
	case <-apply.Done():
	case <-time.After(time.Second):
		t.Fatal("episode driver loss failed to stop admitted apply")
	}
}
