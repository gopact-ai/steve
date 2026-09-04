package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupShipsBuiltinsOnByDefaultButRemembersADisable(t *testing.T) {
	stateDir := t.TempDir()
	m, err := Setup(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := BuiltinNames()
	for _, want := range []string{"skill-creator", "steve", "steve-delegate", "steve-projects", "steve-plans", "steve-memory", "steve-feishu"} {
		if !contains(names, want) {
			t.Fatalf("builtin %s missing from %v", want, names)
		}
		if _, err := os.Stat(filepath.Join(BuiltinRoot(stateDir), want, "SKILL.md")); err != nil {
			t.Fatalf("%s not installed: %v", want, err)
		}
		d := Describe(filepath.Join(BuiltinRoot(stateDir), want))
		if d.Title != want || d.Description == "" {
			t.Fatalf("%s describes itself as %+v", want, d)
		}
	}
	if !contains(m.EnabledNames(), "steve") || !contains(m.EnabledNames(), "skill-creator") {
		t.Fatalf("builtins not enabled: %v", m.EnabledNames())
	}
	// The shipped directory is searched last, so a user's skill of the
	// same name wins.
	paths := m.SearchPaths()
	if paths[len(paths)-1] != BuiltinRoot(stateDir) {
		t.Fatalf("builtin root is not last: %v", paths)
	}
	if err := m.RemovePath(BuiltinRoot(stateDir)); err == nil || !strings.Contains(err.Error(), "ship") {
		t.Fatalf("removing the builtin root: %v", err)
	}
	// Turning one off sticks across a restart; a fresh install rewrites
	// the files but not the choice.
	if err := m.Disable("steve-feishu"); err != nil {
		t.Fatal(err)
	}
	m2, err := Setup(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if contains(m2.EnabledNames(), "steve-feishu") {
		t.Fatal("disabled builtin came back on")
	}
	if !contains(m2.EnabledNames(), "steve") {
		t.Fatal("builtin lost on restart")
	}
	if m2.BuiltinRootPath() != BuiltinRoot(stateDir) {
		t.Fatalf("builtin root = %q", m2.BuiltinRootPath())
	}
}

func TestDescribeReadsBlockScalars(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: folded\ndescription: >\n  first line\n  second line\nother: x\n---\n# Folded\n\nbody\n"), 0o644)
	d := Describe(dir)
	if d.Title != "folded" || d.Description != "first line second line" {
		t.Fatalf("folded = %+v", d)
	}
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: literal\ndescription: |-\n  one\n  two\n---\n"), 0o644)
	if d := Describe(dir); d.Description != "one\ntwo" {
		t.Fatalf("literal = %+v", d)
	}
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# Plain\n\nA paragraph\nthat wraps.\n\nMore.\n"), 0o644)
	if d := Describe(dir); d.Title != "Plain" || d.Description != "A paragraph that wraps." {
		t.Fatalf("plain = %+v", d)
	}
}
