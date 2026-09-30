package node

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestClosedSessionCloseRefusalsDistinguishMissingAndRebound(t *testing.T) {
	for _, tc := range []struct{ name, code string }{{"absent", "absent"}, {"binding", "conflict"}, {"cluster", "forbidden"}, {"not_closed", "unavailable"}} {
		t.Run(tc.name, func(t *testing.T) {
			one := progressSession(t)
			req := nodeSessionRequest(nodewire.SessionActionClose)
			req.ID, req.CommandID = one.record.State.ID, ""
			next := one.copyLocked()
			next.ClusterID, next.Authority = req.Authority.ClusterID, req.Authority
			next.State.Binding, next.State.State, next.State.ProcessStopped = req.Binding, nodewire.SessionClosed, true
			if tc.name == "not_closed" {
				next.State.State = nodewire.SessionIdle
			}
			if err := one.commitLocked(next); err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "absent":
				req.ID = "ns_missing"
			case "binding":
				req.Binding.AttemptID = "another"
			case "cluster":
				req.Authority.ClusterID = "another"
			}
			_, err := one.service.closedState(req)
			var refusal *SessionError
			if !errors.As(err, &refusal) || refusal.Code != tc.code {
				t.Fatalf("close %s = %v, want %s", tc.name, err, tc.code)
			}
		})
	}
}
