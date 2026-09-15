package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

func TestPeerSSHPreviewPicksTheSourceHostTheTargetCanReach(t *testing.T) {
	b, fixture, request, check := peerSSHFixture(t)
	check.SourceHosts = []sshconnect.SourceHost{{Host: "192.0.2.1"}, {Host: "10.4.17.4", Reachable: true}}
	preview, err := b.Preview(t.Context(), request, check)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.previewed.SourceHost != "10.4.17.4" {
		t.Fatalf("preview kept an address the target cannot reach: %#v", fixture.previewed)
	}
	for _, step := range preview.Steps {
		if step.Status == "blocked" {
			t.Fatalf("a reachable address must not block the plan: %#v", preview.Steps)
		}
	}
	hosts, port := b.SourceEndpoints(t.Context())
	if len(hosts) == 0 || hosts[0] != "192.0.2.1" || port != "7711" {
		t.Fatalf("advisor must offer the registered address first: %v %s", hosts, port)
	}
}

func TestPeerSSHPreviewKeepsTheRegisteredHostWhenItIsReachable(t *testing.T) {
	b, fixture, request, check := peerSSHFixture(t)
	check.SourceHosts = []sshconnect.SourceHost{{Host: "192.0.2.1", Reachable: true}, {Host: "10.4.17.4", Reachable: true}}
	if _, err := b.Preview(t.Context(), request, check); err != nil {
		t.Fatal(err)
	}
	if fixture.previewed.SourceHost != "" {
		t.Fatalf("a reachable registered address needs no change: %#v", fixture.previewed)
	}
}

func TestPeerSSHPreviewBlocksWhenTheTargetReachesNoLocalAddress(t *testing.T) {
	b, _, request, check := peerSSHFixture(t)
	check.SourceHosts = []sshconnect.SourceHost{{Host: "192.0.2.1"}, {Host: "192.168.0.30"}}
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
	if blocked == nil || blocked.ID != "source_network" || !strings.Contains(blocked.Message, "192.0.2.1") || !strings.Contains(blocked.Message, "192.168.0.30") {
		t.Fatalf("an unreachable workstation must be named before installation: %#v", preview.Steps)
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

func TestPeerSSHPreviewKeepsAnExplicitHostTheProbeCouldNotReach(t *testing.T) {
	b, fixture, request, check := peerSSHFixture(t)
	request.SourceHost = "192.0.2.1"
	check.SourceHosts = []sshconnect.SourceHost{{Host: "192.0.2.1"}, {Host: "10.4.17.4", Reachable: true}}
	preview, err := b.Preview(t.Context(), request, check)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.previewed.SourceHost != "192.0.2.1" || preview.Script == "" {
		t.Fatalf("an address the user typed must stand: %#v", fixture.previewed)
	}
	var warned bool
	for _, step := range preview.Steps {
		if step.Status == "blocked" {
			t.Fatalf("an explicit address must not be blocked by the probe: %#v", step)
		}
		if step.ID == "source_network" && strings.Contains(step.Message, "连不上") && strings.Contains(step.Message, "192.0.2.1") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the finding must still be shown beside the explicit address: %#v", preview.Steps)
	}
}
