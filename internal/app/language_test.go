package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
)

// Startup and doctor apply the configured projects before any person is
// in the loop, so a failure there is reported in the Hub's language.
func TestStartupAndDoctorReportInTheHubLanguage(t *testing.T) {
	for _, entry := range []struct {
		name string
		run  func(t *testing.T, path string) error
	}{
		{"startup", func(t *testing.T, path string) error {
			application, err := Build(t.Context(), Config{Path: path})
			if application != nil {
				_ = application.Close()
			}
			return err
		}},
		{"doctor", func(t *testing.T, path string) error { return Doctor(path, time.Minute) }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			path := englishHubRefusingProjects(t)
			err := entry.run(t, path)
			want := i18n.New(i18n.LocaleEN).T(i18n.ConfigProjectionPending, "")
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s failure %v is not in the hub's english (%q)", entry.name, err, want)
			}
		})
	}
}

// englishHubRefusingProjects is an English Hub whose ledger refuses to
// record the configured projects.
func englishHubRefusingProjects(t *testing.T) string {
	t.Helper()
	installation, err := desktop.Bootstrap(desktop.Options{StateDir: filepath.Join(t.TempDir(), "desktop")})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(installation.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.Locale = "en"
	if err := config.Save(installation.Paths.Config, cfg); err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Open(filepath.Dir(cfg.Gateway.StatePath), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER refuse_project_declarations BEFORE INSERT ON bindings WHEN NEW.kind = 'project-declarations' BEGIN SELECT RAISE(ABORT, 'injected reconcile failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	return installation.Paths.Config
}
