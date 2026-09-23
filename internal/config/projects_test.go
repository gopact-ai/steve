package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func savedFixture(t *testing.T) (*Config, string) {
	t.Helper()
	cfg := &Config{Agents: map[string]Agent{"main": {Harness: "mock", Default: true}}, Harnesses: map[string]Harness{"mock": {Command: "mock"}}, Projects: map[string]Project{"p": {Home: ProjectHome{Path: "/project"}}}, Gateway: Gateway{HomePath: "/steve-home", DefaultProject: "p"}}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func TestConfigRejectsManualEditFromLoadAndKeepsAdapterDeclarative(t *testing.T) {
	cfg, path := savedFixture(t)
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manual := CloneProjects(cfg)
	manual.Projects["manual"] = Project{Home: ProjectHome{Path: "/manual"}}
	if err := Save(path, manual); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Projects["automatic"] = Project{Home: ProjectHome{Path: "/automatic"}}
	if err := Save(path, loaded); !errors.Is(err, ErrFileChanged) {
		t.Fatalf("online write overwrote manual edit: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(raw) {
		t.Fatal("failed version check changed operator file")
	}
	fresh, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Harnesses["mock"] = Harness{Adapter: "codex-acp", Command: "/derived/runtime/adapter"}
	if err := Save(path, fresh); err != nil {
		t.Fatal(err)
	}
	if raw, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/derived/runtime/adapter") {
		t.Fatal("saved runtime adapter command as an operator declaration")
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("saved adapter declaration cannot restart: %v", err)
	}
}
