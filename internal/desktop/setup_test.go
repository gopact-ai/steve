package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
)

func TestSetupProgressStartsAtTheFirstStepAndSurvivesRestarts(t *testing.T) {
	dir := t.TempDir()
	progress, err := ReadSetup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Done || progress.Step != SetupSteps[0] {
		t.Fatalf("fresh installation should start at %s, got %+v", SetupSteps[0], progress)
	}
	if err := SaveSetup(dir, SetupProgress{Step: "agents"}); err != nil {
		t.Fatal(err)
	}
	progress, err = ReadSetup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Step != "agents" || progress.Done {
		t.Fatalf("saved step should be read back, got %+v", progress)
	}
	if err := SaveSetup(dir, SetupProgress{Step: "finished", Done: true}); err != nil {
		t.Fatal(err)
	}
	progress, _ = ReadSetup(dir)
	if !progress.Done {
		t.Fatalf("done should persist, got %+v", progress)
	}
	if err := SaveSetup(dir, SetupProgress{Step: "agents"}); err != nil {
		t.Fatal(err)
	}
	progress, _ = ReadSetup(dir)
	if !progress.Done || progress.Step != "agents" {
		t.Fatalf("reopening one page after the guide is done keeps it done, got %+v", progress)
	}
	info, err := os.Stat(filepath.Join(dir, setupName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("setup progress must be private, got %#o", info.Mode().Perm())
	}
}

func TestSetupProgressRejectsUnknownSteps(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSetup(dir, SetupProgress{Step: "teleport"}); err == nil || !IsInputError(err) {
		t.Fatalf("unknown step should be rejected as the owner's mistake, got %v", err)
	}
	if err := SaveSetup(dir, SetupProgress{Step: ""}); err == nil {
		t.Fatal("empty step should be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, setupName)); !os.IsNotExist(err) {
		t.Fatalf("rejected progress must not be written: %v", err)
	}
}

func TestSetupProgressTreatsACorruptFileAsAFreshStart(t *testing.T) {
	for _, raw := range []string{"{nope", `{"step": 5, "done": true}`, `{"step": "teleport", "done": true}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, setupName), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		progress, err := ReadSetup(dir)
		if err != nil {
			t.Fatal(err)
		}
		if progress.Step != SetupSteps[0] || progress.Done {
			t.Fatalf("%s should restart the guide, got %+v", raw, progress)
		}
	}
}

func TestPrepareWorkspaceCreatesAPrivateDirectoryUnderTheHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := PrepareWorkspace(i18n.New(i18n.LocaleZH), "~/Steve", filepath.Join(home, "Library", "Application Support", "Steve"))
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "Steve") {
		t.Fatalf("tilde should expand to the home directory, got %s", path)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("workspace should exist as a directory: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("a created workspace is private to its owner, got %#o", info.Mode().Perm())
	}
	again, err := PrepareWorkspace(i18n.New(i18n.LocaleZH), path+"/", "")
	if err != nil || again != path {
		t.Fatalf("an existing directory is accepted as is: %s %v", again, err)
	}
}

func TestPrepareWorkspaceRefusesUnsafePlaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "Library", "Application Support", "Steve")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "etc-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative/dir", "~", home, "/", "/usr/local/steve", "/System/Steve", "/etc/steve", file, string([]byte{'/', 'a', 0}),
		state, filepath.Join(state, "work"), filepath.Dir(state), filepath.Join(home, "Library"), link, filepath.Join(link, "steve")} {
		_, err := PrepareWorkspace(i18n.New(i18n.LocaleZH), path, state)
		if err == nil {
			t.Errorf("%q should be refused", path)
		} else if !IsInputError(err) {
			t.Errorf("%q: refusal should be reported as the owner's input, got %v", path, err)
		}
	}
	if _, err := os.Stat("/usr/local/steve"); err == nil {
		t.Fatal("refused paths must not be created")
	}
}
