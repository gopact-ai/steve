package skills

import (
	"os"
	"path/filepath"
	"slices"
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
	for _, want := range []string{"skill-creator"} {
		if !slices.Contains(names, want) {
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
	if !slices.Contains(m.EnabledNames(), "skill-creator") {
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
	if err := m.Disable("skill-creator"); err != nil {
		t.Fatal(err)
	}
	m2, err := Setup(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(m2.EnabledNames(), "skill-creator") {
		t.Fatal("disabled builtin came back on")
	}
	if m2.BuiltinRootPath() != BuiltinRoot(stateDir) {
		t.Fatalf("builtin root = %q", m2.BuiltinRootPath())
	}
}

func TestPlatformSkillIsNotShippedOrRetainedInRuntimes(t *testing.T) {
	stateDir := t.TempDir()
	root := BuiltinRoot(stateDir)
	legacy := filepath.Join(root, "steve")
	writeSkill(t, root, "steve", "---\nname: steve\ndescription: old platform overview\n---\n# Steve\n")
	m, err := Open(DefaultPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureBuiltins(root, []string{"steve"}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(stateDir, "runtime", "skills")
	if err := m.Materialize(dest); err != nil {
		t.Fatal(err)
	}
	m, err = Setup(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Live{Map: m, Dests: []string{dest}}).Apply(); err != nil {
		t.Fatal(err)
	}
	names, err := BuiltinNames()
	if err != nil || slices.Contains(names, "steve") || slices.Contains(m.EnabledNames(), "steve") {
		t.Fatalf("platform skill remains shipped or enabled: %v, %v", names, err)
	}
	for _, old := range []string{legacy, filepath.Join(dest, "steve")} {
		if _, err := os.Lstat(old); !os.IsNotExist(err) {
			t.Fatalf("retired platform skill remains at %s: %v", old, err)
		}
	}
	refs, err := m.Enabled()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Pack(refs)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range bundle.Skills {
		if entry.Name == "steve" {
			t.Fatal("platform skill still distributed to nodes")
		}
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

func TestStaleBuiltinLeavesTheEnabledSet(t *testing.T) {
	stateDir := t.TempDir()
	m, err := Setup(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// Pretend a previous version shipped and enabled "steve-gone".
	m.mu.Lock()
	data, _ := m.readLocked()
	data.Builtins = append(data.Builtins, "steve-gone")
	data.Enabled = append(data.Enabled, "steve-gone")
	_ = m.writeLocked(data)
	m.mu.Unlock()
	m2, err := Setup(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(m2.EnabledNames(), "steve-gone") {
		t.Fatal("a skill that no longer ships stayed enabled")
	}
	if _, err := m2.Enabled(); err != nil {
		t.Fatalf("enabled set does not resolve: %v", err)
	}
}
