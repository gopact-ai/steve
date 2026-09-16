package sshconnect

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// linkingBackend is a backend whose node is reached through the enrolling
// SSH session: the far end of the tunnel is the program the installation
// puts on the machine, so the tunnel comes up after the installation and
// before the node is expected to have joined.
type linkingBackend struct {
	*fakeBackend
	linkErr   error
	linked    int
	linkedReg Registration
	seen      []string
}

func (b *linkingBackend) Link(_ context.Context, id string, registration Registration) error {
	b.linked++
	b.seen = append(b.seen, id)
	b.linkedReg = registration
	return b.linkErr
}

func linkingFixture(t *testing.T) (*Service, *recordingRunner, *linkingBackend) {
	t.Helper()
	path := configFixture(t, map[string]string{"config": "Host dev\nHostName dev.example\n"})
	runner := &recordingRunner{portOutput: "STEVE_PORT\t25407\tbusy\nSTEVE_PORT\t25408\tfree\nSTEVE_PORT\t25409\tfree\nSTEVE_PORT\tend\tfree\n"}
	backend := &linkingBackend{fakeBackend: &fakeBackend{}}
	svc := New(Options{ConfigPath: path, Runner: runner, Backend: backend})
	t.Cleanup(func() { _ = svc.Close() })
	return svc, runner, backend
}

func TestLinkComesUpAfterTheInstallationAndBeforeConnectivity(t *testing.T) {
	svc, r, b := linkingFixture(t)
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Check.FreeLoopbackPorts; len(got) != 2 || got[0] != 25408 || got[1] != 25409 {
		t.Fatalf("the check did not record the machine's free loopback ports: %v", got)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err != nil || !result.Connected {
		t.Fatalf("commit = %#v %v", result, err)
	}
	if b.linked != 1 || b.linkedReg.Name != "remote" {
		t.Fatalf("the link was not opened once with the registration: %d %+v", b.linked, b.linkedReg)
	}
	var order []string
	for _, step := range result.Steps {
		order = append(order, step.ID)
	}
	joined := strings.Join(order, " ")
	if !strings.Contains(joined, "installation link") || strings.HasSuffix(joined, "link") {
		t.Fatalf("the link did not come between installation and connectivity: %s", joined)
	}
	if !strings.Contains(strings.Join(result.Phases, " "), "installation link connectivity") {
		t.Fatalf("phases do not show the link: %v", result.Phases)
	}
	installs := 0
	for _, call := range r.calls {
		if strings.Contains(call.stdin, "node_token") {
			installs++
		}
	}
	if installs != 1 || b.verifications != 1 {
		t.Fatalf("installed %d times, verified %d times", installs, b.verifications)
	}
}

func TestLinkFailureKeepsTheInstalledNodeAndSkipsConnectivity(t *testing.T) {
	svc, r, b := linkingFixture(t)
	b.linkErr = errors.New("远端链路程序没有启动: EOF")
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err == nil || result.Connected || !result.Registered {
		t.Fatalf("commit = %#v %v", result, err)
	}
	var failed *StepError
	if !errors.As(err, &failed) || failed.Code != "link_failed" || !strings.Contains(failed.Message, "没有启动") {
		t.Fatalf("the failure does not name the link: %v", err)
	}
	installs := 0
	for _, call := range r.calls {
		if strings.Contains(call.stdin, "node_token") {
			installs++
		}
	}
	if installs != 1 || b.verifications != 0 {
		t.Fatalf("installed %d times, verified %d times; the node was installed and connectivity was not checked", installs, b.verifications)
	}
}
