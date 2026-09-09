package skills

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseSourceSpec(t *testing.T) {
	cases := map[string]Source{
		"https://github.com/anthropics/skills/tree/main/skills/pdf": {URL: "https://github.com/anthropics/skills.git", Ref: "main", Subdir: "skills/pdf", Slug: "github.com-anthropics-skills-skills-pdf"},
		"https://github.com/anthropics/skills":                      {URL: "https://github.com/anthropics/skills.git", Slug: "github.com-anthropics-skills"},
		"anthropics/skills":                                         {URL: "https://github.com/anthropics/skills.git", Slug: "github.com-anthropics-skills"},
		"anthropics/skills/skills/xlsx":                             {URL: "https://github.com/anthropics/skills.git", Subdir: "skills/xlsx", Slug: "github.com-anthropics-skills-skills-xlsx"},
		"git@github.com:acme/tools.git":                             {URL: "git@github.com:acme/tools.git", Slug: "github.com-acme-tools"},
	}
	for spec, want := range cases {
		got, err := ParseSourceSpec(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if got.URL != want.URL || got.Ref != want.Ref || got.Subdir != want.Subdir || got.Slug != want.Slug {
			t.Fatalf("%s = %+v, want %+v", spec, got, want)
		}
	}
	for _, bad := range []string{"", "not a source", "a/b/../c"} {
		if _, err := ParseSourceSpec(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestSourcesInstallUpdateAndRemove(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q", "-b", "main")
	_ = os.MkdirAll(filepath.Join(repo, "skills", "alpha"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "skills", "alpha", "SKILL.md"), []byte("---\nname: alpha\ndescription: first\n---\n# alpha\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "one")

	stateDir := t.TempDir()
	m, err := Setup(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	src, err := m.AddSource(context.Background(), "file://"+repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Skills) != 1 || src.Skills[0] != "alpha" || src.Head == "" {
		t.Fatalf("source = %+v", src)
	}
	avail, _ := m.Available()
	var found *Ref
	for i := range avail {
		if avail[i].Name == "alpha" {
			found = &avail[i]
		}
	}
	if found == nil {
		t.Fatalf("alpha not available: %+v", avail)
	}
	if _, err := os.Lstat(found.Path); err != nil || Describe(found.Path).Description != "first" {
		t.Fatalf("alpha path %s does not resolve to the skill", found.Path)
	}
	if _, err := m.AddSource(context.Background(), "file://"+repo); err == nil {
		t.Fatal("installed twice")
	}
	if err := m.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	// Upstream grows a skill; update sees it.
	_ = os.MkdirAll(filepath.Join(repo, "skills", "beta"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "skills", "beta", "SKILL.md"), []byte("# beta\n\nsecond\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "two")
	updated, err := m.UpdateSources(context.Background())
	if err != nil || len(updated) != 1 || updated[0].Error != "" || len(updated[0].Skills) != 2 {
		t.Fatalf("update = %+v err=%v", updated, err)
	}
	if err := m.RemoveSource(src.Slug); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(m.EnabledNames(), "alpha") {
		t.Fatal("alpha still enabled after its source is gone")
	}
	if _, err := os.Stat(src.Dir); !os.IsNotExist(err) {
		t.Fatal("clone survived removal")
	}
	if len(m.Sources()) != 0 || slices.Contains(m.SearchPaths(), src.Root) {
		t.Fatalf("source not forgotten: %v %v", m.Sources(), m.SearchPaths())
	}
}
