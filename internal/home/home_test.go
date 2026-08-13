package home

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBootstrapCreatesFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "home")
	if err := Bootstrap(path, "ou_owner"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("home mode = %o, want 700", info.Mode().Perm())
	}
	for _, name := range []string{FileSoul, FileUser, FileMemory} {
		file := filepath.Join(path, name)
		st, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, st.Mode().Perm())
		}
	}
	user, err := os.ReadFile(filepath.Join(path, FileUser))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(user), "ou_owner") {
		t.Fatalf("USER.md missing owner: %s", user)
	}
	if strings.Contains(string(user), templateOwnerID) {
		t.Fatal("USER.md still has placeholder")
	}
}

func TestBootstrapDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := Bootstrap(dir, "ou_one"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileSoul), []byte("edited soul"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(dir, "ou_two"); err != nil {
		t.Fatal(err)
	}
	soul, err := os.ReadFile(filepath.Join(dir, FileSoul))
	if err != nil {
		t.Fatal(err)
	}
	if string(soul) != "edited soul" {
		t.Fatalf("soul overwritten: %q", soul)
	}
}

func TestLoadOwnerIncludesMemoryNotGuest(t *testing.T) {
	dir := t.TempDir()
	if err := Bootstrap(dir, "ou_owner"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileMemory), []byte("# Memory\nsecret fact"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := Load(dir, ModeOwner)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(owner.Prompt, "secret fact") || !strings.Contains(owner.Prompt, owner.Path) {
		t.Fatalf("owner prompt missing memory or path: %s", owner.Prompt)
	}
	if strings.Contains(owner.Identity, "secret fact") {
		t.Fatal("MEMORY leaked into identity")
	}
	guest, err := Load(dir, ModeGuest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(guest.Prompt, "secret fact") || strings.Contains(guest.Prompt, "USER.md") || strings.Contains(guest.Prompt, "MEMORY.md") {
		t.Fatalf("guest prompt leaked private files: %s", guest.Prompt)
	}
	if strings.Contains(guest.Prompt, dir) || strings.Contains(guest.Prompt, owner.Path) {
		t.Fatalf("guest prompt leaked home path: %s", guest.Prompt)
	}
	if !strings.Contains(guest.Prompt, "Guest context") {
		t.Fatalf("guest wrapper missing: %s", guest.Prompt)
	}
}

func TestLoadMissingHome(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing"), ModeOwner)
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("got %v, want ErrMissing", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := Bootstrap(dir, "ou"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, FileMemory)); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir, ModeOwner)
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("got %v, want ErrMissing", err)
	}
}

func TestTruncateReservesMarker(t *testing.T) {
	long := strings.Repeat("长", BudgetSoul)
	got := truncateTo(long, BudgetSoul)
	if len([]byte(got)) > BudgetSoul {
		t.Fatalf("truncated length %d > %d", len([]byte(got)), BudgetSoul)
	}
	if !strings.HasSuffix(got, TruncationMarker) {
		t.Fatalf("missing marker: %q", got[len(got)-20:])
	}
	if !utf8.ValidString(got) {
		t.Fatal("invalid utf8")
	}
}

func TestTruncateShortUnchanged(t *testing.T) {
	if got := truncateTo("short", BudgetSoul); got != "short" {
		t.Fatalf("got %q", got)
	}
}

func TestSymlinkHomeDirAllowed(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "real")
	if err := Bootstrap(realHome, "ou"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realHome, link); err != nil {
		t.Fatal(err)
	}
	snap, err := Load(link, ModeOwner)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(realHome)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Path != resolved {
		t.Fatalf("path = %q, want %q", snap.Path, resolved)
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	if err := Bootstrap(dir, "ou"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("leaked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, FileMemory)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, FileMemory)); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir, ModeOwner)
	if !errors.Is(err, ErrEscape) {
		t.Fatalf("got %v, want ErrEscape", err)
	}
}

func TestDefaultPathUsesHomeDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	got := DefaultPath()
	want := filepath.Join(root, ".steve", "home")
	if got != want {
		t.Fatalf("DefaultPath = %q, want %q", got, want)
	}
}

func TestDirLoader(t *testing.T) {
	dir := t.TempDir()
	if err := Bootstrap(dir, "ou"); err != nil {
		t.Fatal(err)
	}
	snap, err := (Dir{Path: dir}).Load(ModeGuest)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != ModeGuest || snap.Memory != "" {
		t.Fatalf("unexpected snapshot: %#v", snap)
	}
}

func TestEmptyFileOmitted(t *testing.T) {
	dir := t.TempDir()
	if err := Bootstrap(dir, "ou"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileUser), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := Load(dir, ModeOwner)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snap.Prompt, "# User") {
		t.Fatalf("empty USER.md should be omitted: %s", snap.Prompt)
	}
}
