package sshconnect

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type abandonBackend struct {
	*recoveryBackend
	abandoned  []string
	abandonErr error
}

func (b *abandonBackend) AbandonRegistration(_ context.Context, id string) error {
	b.abandoned = append(b.abandoned, id)
	return b.abandonErr
}

func TestPeerRequestAcceptsDisplayNameAndWorkspaceDir(t *testing.T) {
	svc, _, _, _ := serviceFixture(t)
	svc.installationMode = InstallPeer
	req := installRequest()
	req.Name, req.Level, req.WorkspaceDir = "办公 Linux 盒子", "restricted", "~/steve-workspace"
	plan, err := svc.Plan(t.Context(), req)
	if err != nil || plan.Request.Name != "办公 Linux 盒子" || plan.Request.WorkspaceDir != "~/steve-workspace" {
		t.Fatalf("peer plan refused a display name or workspace: %#v %v", plan.Request, err)
	}
	for _, bad := range []struct{ name, dir string }{{"", "~/x"}, {strings.Repeat("长", 65), "~/x"}, {"ok", "relative/dir"}, {"ok", "~/bad\ndir"}, {"bad\tname", "~/x"}} {
		req.Name, req.WorkspaceDir = bad.name, bad.dir
		if _, err := svc.Plan(t.Context(), req); err == nil {
			t.Fatalf("accepted invalid peer request %q %q", bad.name, bad.dir)
		}
	}
	svc.installationMode = InstallExecutor
	req.Name, req.WorkspaceDir = "办公 Linux 盒子", ""
	if _, err := svc.Plan(t.Context(), req); err == nil {
		t.Fatal("executor node names must keep the config key shape")
	}
}

func TestAbandonDropsPlanAndTellsBackendAboutRegisteredOperation(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	backend := &abandonBackend{recoveryBackend: &recoveryBackend{fakeBackend: b}}
	svc.backend = backend
	r.failInstall = true
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result, err := svc.Commit(t.Context(), plan.ID); err == nil || !result.Registered {
		t.Fatalf("expected partial install %#v %v", result, err)
	}
	if err := svc.Abandon(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if len(backend.abandoned) != 1 || backend.abandoned[0] != plan.ID {
		t.Fatalf("backend was not told to abandon the registered operation: %v", backend.abandoned)
	}
	if _, err := svc.Status(plan.ID); err == nil {
		t.Fatal("abandoned plan still has a status")
	}
	// A plan that never registered anything is only forgotten locally.
	plan, err = svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Abandon(t.Context(), plan.ID); err != nil || len(backend.abandoned) != 1 {
		t.Fatalf("unregistered plan reached the backend: %v %v", err, backend.abandoned)
	}
	// After a restart only the operation record remains; the backend decides.
	id := strings.Repeat("e", 48)
	backend.abandonErr = errors.New("已接入的节点请在资源页移除")
	if err := svc.Abandon(t.Context(), id); err == nil || backend.abandoned[len(backend.abandoned)-1] != id {
		t.Fatalf("backend refusal was swallowed: %v %v", err, backend.abandoned)
	}
	if err := svc.Abandon(t.Context(), "not-an-operation"); err == nil {
		t.Fatal("malformed id reached the backend")
	}
}
