package sshconnect

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type advisingBackend struct {
	*fakeBackend
	hosts []string
	port  string
}

func (b *advisingBackend) SourceEndpoints(context.Context) ([]string, string) { return b.hosts, b.port }

func TestPlanAsksTheTargetWhichLocalAddressesItCanReach(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	svc.backend = &advisingBackend{fakeBackend: b, hosts: []string{"100.82.86.241", "192.168.0.30", "10.4.17.4"}, port: "59398"}
	r.reachOutput = "STEVE_REACH\t10.4.17.4\t1\nSTEVE_REACH\tend\t1\n"
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := []SourceHost{{Host: "100.82.86.241"}, {Host: "192.168.0.30"}, {Host: "10.4.17.4", Reachable: true}}
	if len(plan.Check.SourceHosts) != len(want) {
		t.Fatalf("source hosts: %#v", plan.Check.SourceHosts)
	}
	for i := range want {
		if plan.Check.SourceHosts[i] != want[i] {
			t.Fatalf("source hosts: %#v", plan.Check.SourceHosts)
		}
	}
	var probe *recordedCommand
	for i := range r.calls {
		if strings.Contains(r.calls[i].stdin, "STEVE_REACH") {
			probe = &r.calls[i]
		}
	}
	if probe == nil || probe.args[len(probe.args)-1] != "bash -s" || !strings.Contains(probe.stdin, "59398") || !strings.Contains(probe.stdin, "'10.4.17.4'") {
		t.Fatalf("reverse probe not run over the bound connection: %#v", probe)
	}
}

func TestUnansweredReverseProbeLeavesSourceHostsUnknown(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	svc.backend = &advisingBackend{fakeBackend: b, hosts: []string{"192.168.0.30"}, port: "59398"}
	r.reachOutput = "STEVE_REACH\t192.168.0.30\t1\n"
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Check.SourceHosts != nil {
		t.Fatalf("a probe cut off before its end line must not claim anything: %#v", plan.Check.SourceHosts)
	}
}

func TestReverseProbeScriptOnlyAcceptsPlainHosts(t *testing.T) {
	if _, ok := sourceProbeScript("59398", []string{"10.4.17.4", "bad host'; rm -rf /"}); ok {
		t.Fatal("shell metacharacters reached the probe script")
	}
	if _, ok := sourceProbeScript("59398", nil); ok {
		t.Fatal("an empty candidate list has nothing to probe")
	}
}

type resumingVerifier struct {
	*fakeBackend
	svc      *Service
	attempts int
	live     InstallResult
}

func (b *resumingVerifier) VerifyRegistration(context.Context, string, string) error {
	return &StepError{Stage: "peer_membership", Code: "peer_not_ready", Message: "节点已安装，但集群接入未完成", Suggestion: "稍后继续确认"}
}

func (b *resumingVerifier) ResumeRegistration(_ context.Context, id string) (InstallResult, error) {
	b.attempts++
	b.live, _ = b.svc.Status(id)
	result := InstallResult{PlanID: id, Name: "remote", Registered: true, Status: "needs_attention", Steps: []Step{{ID: "peer_membership", Status: "blocked", Message: "第二次仍未完成", Suggestion: "检查网络"}}}
	return result, errors.New("still blocked")
}

func TestResumeReplacesTheEarlierBlockedStepAndKeepsTheReadyOnes(t *testing.T) {
	svc, _, b, _ := serviceFixture(t)
	backend := &resumingVerifier{fakeBackend: b, svc: svc}
	svc.backend = backend
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.Commit(t.Context(), plan.ID)
	if err == nil || !first.Registered || first.Connected {
		t.Fatalf("expected a blocked first attempt: %#v %v", first, err)
	}
	ready := 0
	for _, step := range first.Steps {
		if step.Status == "ready" {
			ready++
		}
	}
	if ready == 0 {
		t.Fatalf("first attempt settled no steps: %#v", first.Steps)
	}
	second, err := svc.Commit(t.Context(), plan.ID)
	if err == nil || backend.attempts != 1 {
		t.Fatalf("expected the resume to run once and stay blocked: %v", err)
	}
	for _, step := range backend.live.Steps {
		if step.Status == "blocked" {
			t.Fatalf("the earlier blocked step stayed visible while resuming: %#v", backend.live.Steps)
		}
	}
	var blocked []Step
	readyAgain := 0
	for _, step := range second.Steps {
		switch step.Status {
		case "blocked":
			blocked = append(blocked, step)
		case "ready":
			readyAgain++
		}
	}
	if readyAgain != ready || len(blocked) != 1 || blocked[0].Message != "第二次仍未完成" {
		t.Fatalf("resume lost the record or kept the stale failure: %#v", second.Steps)
	}
}
