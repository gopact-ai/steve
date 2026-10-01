package readmodel

import (
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
)

func TestLiveAttemptCarriesDurableForceStop(t *testing.T) {
	s := newSourceFixture()
	s.live = []attempt.Record{{Spec: attempt.Spec{ID: "old", TaskID: "1"}, State: attempt.Failed, Unsettled: true, ForceStop: &attempt.ForceStop{Revision: 3, Level: "exhausted", Reason: "stop_unproven"}}}
	adapter := s.adapter()
	got, err := adapter.LiveAttempts(t.Context())
	if err != nil || len(got) != 1 || got[0].ForceStop == nil || got[0].ForceStop.Revision != 3 || got[0].ForceStop.Reason != "stop_unproven" {
		t.Fatalf("force stop disappeared from snapshot: %+v %v", got, err)
	}
}
