package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// The machine reaches this node through the enrolling SSH session, at two
// loopback ports of its own. They come from what the machine reported
// free, skipping the ports its own node will bind on every interface, and
// the plan says so; the same check always yields the same ports, so the
// reviewed plan and the registered one agree.
func TestPeerSSHPreviewRoutesTheMachineToThisNodeThroughTheSession(t *testing.T) {
	b, fixture, request, check := peerSSHFixture(t)
	request.Addr, request.RaftAddr = "192.0.2.5:25407", ""
	check.FreeLoopbackPorts = []int{25407, 25408, 25409, 25410, 25411}
	preview, err := b.Preview(t.Context(), request, check)
	if err != nil {
		t.Fatal(err)
	}
	want := coordination.Route{Raft: "127.0.0.1:25409", API: "127.0.0.1:25410"}
	if fixture.previewed.HubRoute != want {
		t.Fatalf("the tunnel ports were not chosen around the node's own: %+v", fixture.previewed.HubRoute)
	}
	var described bool
	for _, step := range preview.Steps {
		if step.Status == "blocked" {
			t.Fatalf("a routed plan must not be blocked: %#v", step)
		}
		described = described || step.ID == "peer_link" && strings.Contains(step.Message, "25409") && strings.Contains(step.Message, "25410")
	}
	if !described || preview.Script == "" {
		t.Fatalf("the plan does not tell the user about the tunnel: %#v", preview.Steps)
	}
	again, err := b.Preview(t.Context(), request, check)
	if err != nil || again.ReviewID != preview.ReviewID {
		t.Fatalf("the same check gave a different plan: %v", err)
	}
}

func TestPeerSSHPreviewBlocksWhenTheMachineHasNoPortsForTheSession(t *testing.T) {
	b, _, request, check := peerSSHFixture(t)
	for _, free := range [][]int{nil, {25407}} {
		check.FreeLoopbackPorts = free
		preview, err := b.Preview(t.Context(), request, check)
		if err != nil {
			t.Fatal(err)
		}
		var blocked *sshconnect.Step
		for i := range preview.Steps {
			if preview.Steps[i].Status == "blocked" {
				blocked = &preview.Steps[i]
			}
		}
		if blocked == nil || blocked.ID != "peer_link" || preview.Script != "" {
			t.Fatalf("free=%v: a machine without tunnel ports must block before installation: %#v", free, preview.Steps)
		}
	}
}

func TestVerifyRegistrationNamesTheRealErrorAndStopsOnlyOnStall(t *testing.T) {
	b, fixture, _, _ := peerSSHFixture(t)
	fixture.completeResult = PeerEnrollmentResult{NodeID: "node-new", Name: "remote", Phase: "synchronizing", Error: `节点 node-new 无法完成独立互联验证：Post "https://192.0.2.1:7711/cluster/network/check": dial tcp: i/o timeout`}
	fixture.completeError = errors.New("coordination: target has not caught up")
	b.stallLimit = 300 * time.Millisecond
	var reports []string
	ctx := sshconnect.WithReporter(t.Context(), func(message string) { reports = append(reports, message) })
	started := time.Now()
	err := b.VerifyRegistration(ctx, "remote", strings.Repeat("a", 48))
	var step *sshconnect.StepError
	if !errors.As(err, &step) || step.Code != "peer_not_ready" || !strings.Contains(step.Suggestion, "i/o timeout") {
		t.Fatalf("stall must surface the enrollment error: %v", err)
	}
	if time.Since(started) < b.stallLimit || fixture.completes < 2 {
		t.Fatalf("verification gave up before the stall limit: %d polls in %s", fixture.completes, time.Since(started))
	}
	joined := strings.Join(reports, "\n")
	if !strings.Contains(joined, "i/o timeout") || strings.Count(joined, "i/o timeout") != 1 {
		t.Fatalf("the log must name the failing check once: %q", joined)
	}
}

func TestVerifyRegistrationKeepsWaitingWhilePhasesAdvance(t *testing.T) {
	b, fixture, _, _ := peerSSHFixture(t)
	b.stallLimit = 1200 * time.Millisecond
	phases := []string{"awaiting_peer", "synchronizing", "joined", "ready"}
	fixture.completeResult = PeerEnrollmentResult{NodeID: "node-new", Name: "remote", Phase: phases[0]}
	fixture.completeError = errors.New("not yet")
	fixture.advance = func(f *sshEnrollmentFixture) {
		// Each phase lasts two polls, longer than the stall limit would
		// allow a phase to go without any change at all.
		if f.completes%2 != 0 {
			return
		}
		next := 0
		for i, phase := range phases {
			if phase == f.completeResult.Phase {
				next = i + 1
			}
		}
		if next < len(phases) {
			f.completeResult.Phase = phases[next]
			f.completeResult.Ready, f.completeError = phases[next] == "ready", errors.New("not yet")
			if f.completeResult.Ready {
				f.completeError = nil
			}
		}
	}
	if err := b.VerifyRegistration(context.Background(), "remote", strings.Repeat("a", 48)); err != nil {
		t.Fatalf("advancing phases must not be treated as a stall: %v", err)
	}
}
