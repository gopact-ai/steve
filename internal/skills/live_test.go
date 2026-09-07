package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddDestsMaterializesSelectedRuntimeWithoutRestartingAgents(t *testing.T) {
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
	var restarts int
	live := &Live{Map: m, After: func() error { restarts++; return nil }}
	dest := filepath.Join(root, "selected-runtime")
	if err := live.AddDests(dest, dest); err != nil {
		t.Fatal(err)
	}
	if len(live.Dests) != 1 || live.Dests[0] != dest || restarts != 0 {
		t.Fatalf("runtime registration: dests=%v restarts=%d", live.Dests, restarts)
	}
	if _, err := os.Readlink(filepath.Join(dest, "remind")); err != nil {
		t.Fatal(err)
	}
	if err := live.Disable("remind"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatal("later skill change did not retain normal restart behavior")
	}
	if _, err := os.Lstat(filepath.Join(dest, "remind")); !os.IsNotExist(err) {
		t.Fatal("later skill change did not reach newly registered runtime")
	}
}

func TestLiveApplyOnChangeOnly(t *testing.T) {
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
	dest := filepath.Join(root, "runtime")
	var restarts int
	live := &Live{
		Map:   m,
		Dests: []string{dest},
		After: func() error {
			restarts++
			return nil
		},
	}
	if err := live.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatalf("restarts = %d, want 1", restarts)
	}
	if _, err := os.Readlink(filepath.Join(dest, "remind")); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatalf("idempotent enable restarted: %d", restarts)
	}
	if err := live.Disable("remind"); err != nil {
		t.Fatal(err)
	}
	if restarts != 2 {
		t.Fatalf("restarts after disable = %d, want 2", restarts)
	}
	if _, err := os.Stat(filepath.Join(dest, "remind")); !os.IsNotExist(err) {
		t.Fatal("disabled skill still linked")
	}
}

func TestLiveAddPathDoesNotRestart(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, extra, "calendar", "cal")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	var restarts int
	live := &Live{
		Map:   m,
		Dests: []string{filepath.Join(root, "runtime")},
		After: func() error {
			restarts++
			return nil
		},
	}
	if err := live.AddPath(extra); err != nil {
		t.Fatal(err)
	}
	if restarts != 0 {
		t.Fatalf("add path restarted: %d", restarts)
	}
	if err := live.Enable("calendar"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatalf("enable after path add restarts = %d", restarts)
	}
}
