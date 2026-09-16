package sshconnect

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// linkingBackend is a backend whose node is reached through the enrolling
// SSH session: the installation must bring the tunnel up after the
// registration and before anything is sent to the machine.
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
	runner := &recordingRunner{portOutput: "STEVE_PORT\t59407\tbusy\nSTEVE_PORT\t59408\tfree\nSTEVE_PORT\t59409\tfree\nSTEVE_PORT\tend\tfree\n"}
	backend := &linkingBackend{fakeBackend: &fakeBackend{}}
	svc := New(Options{ConfigPath: path, Runner: runner, Backend: backend})
	t.Cleanup(func() { _ = svc.Close() })
	return svc, runner, backend
}

func TestLinkComesUpAfterRegistrationAndBeforeTheMachineIsTouched(t *testing.T) {
	svc, r, b := linkingFixture(t)
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Check.FreeLoopbackPorts; len(got) != 2 || got[0] != 59408 || got[1] != 59409 {
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
	if !strings.Contains(joined, "registration link installation") {
		t.Fatalf("the link did not come between registration and installation: %s", joined)
	}
	if !strings.Contains(strings.Join(result.Phases, " "), "registration link installation") {
		t.Fatalf("phases do not show the link: %v", result.Phases)
	}
	// The install script is the last thing sent; the link comes before it.
	installs := 0
	for _, call := range r.calls {
		if strings.Contains(call.stdin, "node_token") {
			installs++
		}
	}
	if installs != 1 {
		t.Fatalf("installed %d times", installs)
	}
}

func TestLinkFailureKeepsTheRegistrationAndInstallsNothing(t *testing.T) {
	svc, r, b := linkingFixture(t)
	b.linkErr = errors.New("remote port forwarding failed for listen port 59408")
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err == nil || result.Connected || !result.Registered {
		t.Fatalf("commit = %#v %v", result, err)
	}
	var failed *StepError
	if !errors.As(err, &failed) || failed.Code != "link_failed" || !strings.Contains(failed.Message, "59408") {
		t.Fatalf("the failure does not name the link: %v", err)
	}
	for _, call := range r.calls {
		if strings.Contains(call.stdin, "node_token") {
			t.Fatal("the machine was installed without a link")
		}
	}
}
