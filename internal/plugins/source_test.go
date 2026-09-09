package plugins

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Plugin Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Plugin Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitCommitAndDirectoryProduceSamePinnedPackage(t *testing.T) {
	dir := fixtureDirectory(t)
	before, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "init", "--quiet", "--template=")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "--quiet", "-m", "initial")
	commit := gitTest(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "skills/review/SKILL.md"), []byte("dirty source"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "--quiet", "-m", "changed")
	source := Source{Kind: "git", Location: dir, Commit: commit}
	fetched, resolved, err := Resolve(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != source || fetched.Digest != before.Digest || !bytes.Equal(fetched.Data, before.Data) {
		t.Fatal("Git read followed HEAD or changed canonical bytes")
	}
}

func TestGitPackageSubdirectoryAndLinks(t *testing.T) {
	root := t.TempDir()
	source := fixtureDirectory(t)
	sub := filepath.Join(root, "packages", "github")
	if err := os.MkdirAll(filepath.Dir(sub), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, sub); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "init", "--quiet", "--template=")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "--quiet", "-m", "package")
	before, err := ReadDirectory(t.Context(), sub)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Resolve(t.Context(), Source{Kind: "git", Location: root, Commit: gitTest(t, root, "rev-parse", "HEAD"), Subdir: "packages/github"})
	if err != nil || bundle.Digest != before.Digest {
		t.Fatalf("subdir: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(sub, "link")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "--quiet", "-m", "link")
	if _, _, err := Resolve(t.Context(), Source{Kind: "git", Location: root, Commit: gitTest(t, root, "rev-parse", "HEAD"), Subdir: "packages/github"}); err == nil {
		t.Fatal("Git symlink accepted")
	}
}

func TestGitRejectsUnpinnedOrExecutableSources(t *testing.T) {
	for _, source := range []Source{
		{Kind: "git", Location: "https://example.invalid/repo", Commit: "main"},
		{Kind: "git", Location: "https://user:secret@example.invalid/repo", Commit: strings.Repeat("a", 40)},
		{Kind: "git", Location: "ext::sh command", Commit: strings.Repeat("a", 40)},
		{Kind: "git", Location: "ssh://example.invalid/repo", Commit: strings.Repeat("a", 40)},
		{Kind: "git", Location: "https://example.invalid/repo", Commit: strings.Repeat("a", 40), Subdir: "../outside"},
	} {
		if _, _, err := Resolve(t.Context(), source); err == nil {
			t.Fatalf("accepted %+v", source)
		}
	}
}

func TestCanonicalBundleIgnoresFileTimesAndManifestWhitespace(t *testing.T) {
	dir := fixtureDirectory(t)
	first, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), append([]byte(" \n"), raw...), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, ManifestFile), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	second, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || !bytes.Equal(first.Data, second.Data) {
		t.Fatal("metadata changed canonical package")
	}
}

func TestGitObjectSizeLimitAppliesToProcessOutput(t *testing.T) {
	dir := fixtureDirectory(t)
	if err := os.WriteFile(filepath.Join(dir, "too-large"), make([]byte, MaxFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "init", "--quiet", "--template=")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "--quiet", "-m", "large object")
	_, _, err := Resolve(t.Context(), Source{Kind: "git", Location: dir, Commit: gitTest(t, dir, "rev-parse", "HEAD")})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("large Git object: %v", err)
	}
}
