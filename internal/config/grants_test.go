package config

import (
	"testing"

	"github.com/gopact-ai/steve/internal/project"
)

func TestConfiguredGrantsReconcileAsAnExplicitOverlay(t *testing.T) {
	cfg, path, controller, book := controllerFixture(t)
	if _, err := controller.Store.Grant(t.Context(), "p", "guest", project.RoleAdmin, "operator"); err != nil {
		t.Fatal(err)
	}
	_, beforeHash, err := ProjectDeclarations(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Projects["p"]
	p.Grants = map[string]string{"guest": "none", "another": "read"}
	cfg.Projects["p"] = p
	_, afterHash, err := ProjectDeclarations(cfg)
	if err != nil || beforeHash == afterHash {
		t.Fatal("configured grants do not participate in declaration identity")
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	startup := ProjectController{Store: project.Open(book)}
	if err := startup.Reconcile(t.Context(), loaded); err != nil {
		t.Fatal(err)
	}
	role, err := startup.Store.Access(t.Context(), "p", "guest", "owner")
	if err != nil || role != project.RoleNone {
		t.Fatalf("configured deny did not override runtime grant: %s %v", role, err)
	}
	role, err = startup.Store.Access(t.Context(), "p", "another", "owner")
	if err != nil || role != project.RoleRead {
		t.Fatalf("configured read lost: %s %v", role, err)
	}
	p = loaded.Projects["p"]
	p.Grants = nil
	loaded.Projects["p"] = p
	if err := Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	if err := startup.Reconcile(t.Context(), loaded); err != nil {
		t.Fatal(err)
	}
	role, err = startup.Store.Access(t.Context(), "p", "guest", "owner")
	if err != nil || role != project.RoleAdmin {
		t.Fatalf("removing config grant destroyed independent runtime grant: %s %v", role, err)
	}
	role, err = startup.Store.Access(t.Context(), "p", "another", "owner")
	if err != nil || role != project.RoleWrite {
		t.Fatalf("removing config-only grant failed to reveal default: %s %v", role, err)
	}
}

func TestInvalidConfiguredGrantFailsBeforeCommit(t *testing.T) {
	cfg, _, controller, _ := controllerFixture(t)
	for _, grants := range []map[string]string{{"guest": "root"}, {" ": "none"}, {"guest": "none", " guest ": "write"}} {
		candidate := CloneProjects(cfg)
		p := candidate.Projects["p"]
		p.Grants = grants
		candidate.Projects["p"] = p
		writes := 0
		if err := controller.Commit(t.Context(), candidate, func() error { writes++; return nil }); err == nil || writes != 0 {
			t.Fatalf("invalid grant reached commit: %v", err)
		}
	}
}
