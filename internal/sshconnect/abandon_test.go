package sshconnect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
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
	if _, err := svc.Status(t.Context(), plan.ID); err == nil {
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
	// Giving up is idempotent: an operation the backend no longer has is
	// already abandoned, so the dialog can forget its copy.
	backend.abandonErr = Fail(i18n.Catalog{}, "planning", "unknown_plan", "这次接入的记录已不存在", "刷新接入记录")
	if err := svc.Abandon(t.Context(), strings.Repeat("f", 48)); err != nil {
		t.Fatalf("a vanished operation was not treated as abandoned: %v", err)
	}
}
