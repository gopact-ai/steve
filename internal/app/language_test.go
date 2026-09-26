package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
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

// A project memory the owner has not written yet is offered from a
// template that explains itself in the Hub's language, whichever
// authority keeps the memory.
func TestProjectMemoryTemplateIsInTheHubLanguage(t *testing.T) {
	root := t.TempDir()
	book, err := ledger.Open(filepath.Join(root, "ledger"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	cfg := &config.Config{Gateway: config.Gateway{Locale: "en", HomePath: filepath.Join(root, "home"), StatePath: filepath.Join(root, "state.json")}}
	for name, authority := range map[string]*ledger.Ledger{"local files": nil, "shared ledger": book} {
		prepared, err := prepareApplicationMemoryWithSettings(t.Context(), cfg, authority, config.NewRuntimeSettings(cfg))
		if err != nil {
			t.Fatal(err)
		}
		text, err := prepared.Service.Text(t.Context(), memory.ProjectScope("p"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(line, "#") && strings.IndexFunc(line, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
				t.Errorf("%s: an english hub's project memory template says %q", name, line)
			}
		}
	}
}
