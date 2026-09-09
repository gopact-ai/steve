package node

import (
	"os"
	"path/filepath"
	"testing"

	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
)

// An apply whose staging directory cannot be cleared must fail rather
// than unpack over the leftovers: Unpack writes its own entries and says
// nothing about what was already there, so a mixed directory would be
// promoted to the bundle's hash and every stale skill in it linked into
// the harness homes, while the manifest the node reports names only the
// bundle's own skills.
func TestSkillsApplyRefusesStagingItCannotClear(t *testing.T) {
	bin := buildMockAgent(t)
	state := t.TempDir()
	server := startNode(t, ServerConfig{
		Name: "host-staging", Token: "tok", StateDir: state,
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}},
	})
	registry := NewRegistry("hub-1", map[string]Config{"host-staging": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	if _, err := registry.Advert(t.Context(), "host-staging"); err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "deploy", "SKILL.md"), []byte("# deploy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := skills.Pack([]skills.Ref{{Name: "deploy", Path: filepath.Join(src, "deploy")}})
	if err != nil {
		t.Fatal(err)
	}

	// A skill left by an earlier attempt, inside a directory the node may
	// not write: RemoveAll cannot unlink the file it holds.
	stale := filepath.Join(state, "skills", bundle.Hash+".staging", "stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "SKILL.md"), []byte("# stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stale, 0o700)
		// Should the apply promote the staging directory again, TempDir
		// still has to be able to remove what it left behind.
		_ = os.Chmod(filepath.Join(state, "skills", bundle.Hash, "stale"), 0o700)
	})

	if err := registry.PushSkills(t.Context(), "host-staging", bundle); err == nil {
		t.Fatal("apply succeeded over a staging directory it could not clear")
	}
	for _, dest := range steveruntime.SelectedSkillDests(state, []string{"codex"}) {
		if _, err := os.Lstat(filepath.Join(dest, "stale")); !os.IsNotExist(err) {
			t.Errorf("stale skill linked into %s: %v", dest, err)
		}
	}
	if got := server.currentSkills(); got != "" {
		t.Errorf("current skills = %q after a failed apply", got)
	}
}
