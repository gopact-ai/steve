package configbuild

import (
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// A project change that cannot be saved, or saved but not yet applied, is
// explained in the language of the person who made it.
func TestProjectCommitIsExplainedInTheRequestLanguage(t *testing.T) {
	for _, tc := range []struct {
		locale i18n.Locale
		wantEN bool
	}{{i18n.LocaleEN, true}, {i18n.LocaleZH, false}} {
		t.Run(string(tc.locale), func(t *testing.T) {
			cfg, path, controller, book := controllerFixture(t)
			ctx := i18n.WithLocale(t.Context(), tc.locale)
			empty := config.CloneProjects(cfg)
			empty.Projects = map[string]config.Project{}
			err := controller.Commit(ctx, empty, func() error { return nil })
			if err == nil || containsHan(err.Error()) == tc.wantEN {
				t.Fatalf("empty project list refusal %q is not in %s", err, tc.locale)
			}
			candidate := config.CloneProjects(cfg)
			candidate.Projects["new"] = config.Project{Home: config.ProjectHome{Path: "/new"}}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER block_project_reconcile BEFORE INSERT ON bindings WHEN NEW.kind = 'project-declarations' BEGIN SELECT RAISE(ABORT, 'injected reconcile failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			err = controller.Commit(ctx, candidate, func() error { return config.Save(path, candidate) })
			var pending *ProjectionPendingError
			if !errors.As(err, &pending) {
				t.Fatalf("pending projection lost: %v", err)
			}
			if said := strings.TrimSuffix(pending.Error(), pending.Err.Error()); containsHan(said) == tc.wantEN {
				t.Fatalf("pending projection %q is not in %s", pending.Error(), tc.locale)
			}
		})
	}
}
