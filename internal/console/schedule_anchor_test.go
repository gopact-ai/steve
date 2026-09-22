package console

import (
	"sync/atomic"
	"testing"
)

func TestScheduleControlDoesNotReplaceRunningMessageAnchor(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 4)}
	s := New(h, "owner", nil)
	var anchors atomic.Int32
	s.SetAnchorer(func(string, string, string) { anchors.Add(1) })
	original := enqueueForTest(t, s, "work", "keep working")
	running := nextCall(t, h)
	defer func() { running.finish <- nil; awaitExchange(t, s, original.ID) }()
	for _, input := range []string{"/schedules", "@builder /every 30m check CI", "/at 1h remind"} {
		control := enqueueForTest(t, s, "work", input)
		call := nextCall(t, h)
		call.finish <- nil
		awaitExchange(t, s, control.ID)
		if anchors.Load() != 1 {
			t.Fatalf("%s replaced the running turn's anchor", input)
		}
	}
}
