package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// Codex creates skills/.system/ when a session starts. Reopening a plugin
// runtime must still match the digest recorded when it was prepared.
func TestSkillsDigestIgnoresHarnessHiddenState(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "review"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review", "SKILL.md"), []byte("---\nname: review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := SkillsDigest(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".system", "skill-creator"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".system/.codex-system-skills.marker", ".system/skill-creator/SKILL.md", ".DS_Store"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	after, err := SkillsDigest(t.Context(), dir)
	if err != nil {
		t.Fatalf("harness state made the runtime unreadable: %v", err)
	}
	if after != before {
		t.Fatal("harness state changed the skills digest")
	}
	if err := os.WriteFile(filepath.Join(dir, "review", "SKILL.md"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, _ := SkillsDigest(t.Context(), dir); changed == before {
		t.Fatal("a real skill change kept the same digest")
	}
}
