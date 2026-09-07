package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
)

func TestSharedMemoryActivationImportsOnlyPortableDocsThenIgnoresLocalFiles(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "home")
	if err := home.Bootstrap(local, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := home.Write(local, home.FileSoul, "shared soul"); err != nil {
		t.Fatal(err)
	}
	if err := home.Write(local, home.FileMemory, "# Memory\n- original fact\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "auth.json"), []byte("private credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Open(filepath.Join(root, "ledger"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	cfg := &config.Config{Gateway: config.Gateway{HomePath: local, StatePath: filepath.Join(root, "state.json")}, Projects: map[string]config.Project{"work": {}}}
	first, err := prepareApplicationMemory(t.Context(), cfg, book)
	if err != nil {
		t.Fatal(err)
	}
	if first.Shared == nil || first.Service.AuditPath() != "" || first.Service.Where(memory.Global) != "" {
		t.Fatal("shared memory still uses a local authority")
	}
	if err := first.Service.Replace(t.Context(), memory.Global, "# Memory\n- saved in UI\n", memory.Actor{By: "console"}); err != nil {
		t.Fatal(err)
	}
	localRaw, err := os.ReadFile(filepath.Join(local, home.FileMemory))
	if err != nil || strings.Contains(string(localRaw), "saved in UI") {
		t.Fatal("shared edit was also written to Markdown")
	}
	if err := os.RemoveAll(local); err != nil {
		t.Fatal(err)
	}
	second, err := prepareApplicationMemory(t.Context(), cfg, book)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := second.Home.Load(home.ModeOwner)
	if err != nil || snapshot.Soul != "shared soul" || !strings.Contains(snapshot.Memory, "saved in UI") || strings.Contains(snapshot.Prompt, "private credential") {
		t.Fatalf("reconstructed shared identity: %+v %v", snapshot, err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatal("shared activation recreated local identity files")
	}
}

func TestSharedMemoryBootstrapDoesNotFollowProjectFileSymlinks(t *testing.T) {
	root := t.TempDir()
	credential := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(credential, []byte("must not import credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectFiles := filepath.Join(root, "memory", "projects")
	if err := os.MkdirAll(projectFiles, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(credential, filepath.Join(projectFiles, "work.md")); err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Open(filepath.Join(root, "ledger"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	cfg := &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(root, "state.json")}, Projects: map[string]config.Project{"work": {}}}
	if _, err := prepareApplicationMemory(t.Context(), cfg, book); err == nil {
		t.Fatal("bootstrap followed a project memory symlink")
	}
	items, err := memory.NewLedgerStore(book).List(t.Context(), memory.ProjectScope("work"))
	if err != nil || len(items) != 0 {
		t.Fatalf("rejected bootstrap stored a credential: %+v %v", items, err)
	}
}
