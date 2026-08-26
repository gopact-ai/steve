package skills

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMapEnableDisableAndMaterialize(t *testing.T) {
	root := t.TempDir()
	search := filepath.Join(root, "catalog")
	writeSkill(t, search, "remind", "remind people")
	writeSkill(t, search, "weather", "weather")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("missing"); err == nil {
		t.Fatal("expected unknown skill")
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 1 || enabled[0].Name != "remind" {
		t.Fatalf("enabled = %#v, %v", enabled, err)
	}
	dest := filepath.Join(root, "runtime")
	if err := m.Materialize(dest); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(dest, "remind"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(search, "remind") {
		t.Fatalf("link = %q", target)
	}
	if _, err := os.Stat(filepath.Join(dest, "weather")); !os.IsNotExist(err) {
		t.Fatal("disabled skill was materialized")
	}
	if err := m.Disable("remind"); err != nil {
		t.Fatal(err)
	}
	if err := m.Materialize(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "remind")); !os.IsNotExist(err) {
		t.Fatal("disabled skill still linked")
	}
}

func TestMapEnableAbsolutePath(t *testing.T) {
	root := t.TempDir()
	outside := writeSkill(t, t.TempDir(), "lark-im", "im")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(outside); err != nil {
		t.Fatal(err)
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 1 || enabled[0].Name != "lark-im" {
		t.Fatalf("enabled = %#v, %v", enabled, err)
	}
}

func TestMapSearchPathAndAvailable(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, extra, "calendar", "cal")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddPath(extra); err != nil {
		t.Fatal(err)
	}
	avail, err := m.Available()
	if err != nil {
		t.Fatal(err)
	}
	if len(avail) != 1 || avail[0].Name != "calendar" {
		t.Fatalf("available = %#v", avail)
	}
	if err := m.Enable("calendar"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemovePath(extra); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("calendar"); err == nil {
		t.Fatal("expected skill to disappear after path removal")
	}
}

func TestFingerprintStableAndOrdered(t *testing.T) {
	root := t.TempDir()
	search := filepath.Join(root, "catalog")
	writeSkill(t, search, "a", "a")
	writeSkill(t, search, "b", "b")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("b"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("a"); err != nil {
		t.Fatal(err)
	}
	first := m.Fingerprint()
	names := m.EnabledNames()
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("names = %v", names)
	}
	if m.Fingerprint() != first {
		t.Fatal("fingerprint unstable")
	}
	if err := m.Disable("a"); err != nil {
		t.Fatal(err)
	}
	if m.Fingerprint() == first {
		t.Fatal("fingerprint did not change")
	}
}

func TestMapDisableByBasenameAfterAbsEnable(t *testing.T) {
	root := t.TempDir()
	outside := writeSkill(t, t.TempDir(), "lark-im", "im")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(outside); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable("lark-im"); err != nil {
		t.Fatal(err)
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 0 {
		t.Fatalf("enabled = %#v, %v", enabled, err)
	}
}

func TestMapEnableDedupesPathAndName(t *testing.T) {
	root := t.TempDir()
	search := filepath.Join(root, "catalog")
	writeSkill(t, search, "remind", "remind")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(filepath.Join(search, "remind")); err != nil {
		t.Fatal(err)
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 1 || enabled[0].Name != "remind" {
		t.Fatalf("enabled = %#v, %v", enabled, err)
	}
}

func TestRemovePathPrunesEnabled(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, extra, "calendar", "cal")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddPath(extra); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("calendar"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemovePath(extra); err != nil {
		t.Fatal(err)
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 0 {
		t.Fatalf("enabled after path removal = %#v, %v", enabled, err)
	}
}
