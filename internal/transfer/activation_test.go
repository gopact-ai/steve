package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestPendingImportGatesStartupUntilConfigurationAndActivationComplete(t *testing.T) {
	source, _ := transferFixture(t)
	path := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	for _, configSaved := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-config-save", true: "after-config-save"}[configSaved], func(t *testing.T) {
			target := t.TempDir()
			home := filepath.Join(t.TempDir(), "home")
			cfg := &config.Config{Projects: map[string]config.Project{"q": {Home: config.ProjectHome{Path: t.TempDir()}}}}
			persist := func(p project.Project) {
				cfg.Projects[p.ID] = config.Project{Home: config.ProjectHome{Path: p.Home.Path}, Level: string(p.Level)}
			}
			failure := errors.New("configuration completion interrupted")
			opts := ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: path, Home: home, Finalize: func(p project.Project) error {
				if configSaved {
					persist(p)
				}
				return failure
			}}
			if _, err := Import(t.Context(), opts); !errors.Is(err, failure) {
				t.Fatal("import did not stop at configuration boundary", err)
			}
			book, err := ledger.Open(target, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			projects := project.Open(book)
			projects.SetHubID("target")
			owner, found, err := projects.Ownership(t.Context(), "p")
			if err != nil || !found || owner.State != "importing" {
				t.Fatalf("incomplete configuration activated ownership: %+v %v", owner, err)
			}
			if err := (config.ProjectController{Store: projects}).Reconcile(t.Context(), cfg); !errors.Is(err, project.ErrTransferPending) {
				t.Fatal("startup did not gate incomplete import", err)
			}
			if _, found, err := projects.Lookup(t.Context(), "p"); err != nil || !found {
				t.Fatal("startup discarded imported project metadata", err)
			}
			if _, _, err := projects.Get(t.Context(), "p"); !errors.Is(err, project.ErrNotOwner) {
				t.Fatal("incomplete import can execute", err)
			}
			book.Close()
			opts.Finalize = func(p project.Project) error { persist(p); return nil }
			if _, err := Import(t.Context(), opts); err != nil {
				t.Fatal("same-payload completion failed", err)
			}
			book, err = ledger.Open(target, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			projects = project.Open(book)
			projects.SetHubID("target")
			if err := (config.ProjectController{Store: projects}).Reconcile(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if _, found, err := projects.Get(t.Context(), "p"); err != nil || !found {
				t.Fatal("completed import not executable", err)
			}
		})
	}
}

func TestSourceRetainsOwnershipWhenBundleAncestorSyncFails(t *testing.T) {
	source, _ := transferFixture(t)
	out := filepath.Join(t.TempDir(), "new", "nested", "bundle")
	failure := errors.New("directory sync failed")
	_, err := exportProject(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: out, Evidence: "stopped"}, func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return failure
		}
		return f.Sync()
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	projects.SetHubID("source")
	if _, found, err := projects.Get(t.Context(), "p"); err != nil || !found {
		t.Fatal("source released without a durable bundle", err)
	}
}

func TestMigratedPendingDisclosureIsInterruptedWithoutSealedBody(t *testing.T) {
	source, _ := transferFixture(t)
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	if err := projects.ProposeDisclosure(t.Context(), project.DisclosureRequest{ID: "disc-1", Project: "p", TaskID: "1", ConversationID: "console:a", Requester: "owner", Bytes: 42}); err != nil {
		t.Fatal(err)
	}
	book.Close()
	path := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if _, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: path, Home: filepath.Join(t.TempDir(), "home")}); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects = project.Open(book)
	if pending, err := projects.PendingDisclosures(t.Context()); err != nil || len(pending) != 0 {
		t.Fatal("target advertises approval without the held content", pending, err)
	}
	op, found, err := book.Operation(t.Context(), "disc-1")
	if err != nil || !found || op.State != project.DisclosureInterrupted {
		t.Fatal("disclosure history not explicitly interrupted", op, err)
	}
	events, err := book.Events(t.Context(), "disc-1")
	if err != nil || len(events) < 2 || events[len(events)-1].To != project.DisclosureInterrupted {
		t.Fatal("interruption transition absent from history", events, err)
	}
}
