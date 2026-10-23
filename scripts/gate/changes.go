package main

import (
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strings"
)

// changedFiles is every path that differs from the merge base with base:
// committed on this branch, staged, unstaged or untracked.
func changedFiles(base string) ([]string, error) {
	seen := map[string]bool{}
	add := func(args ...string) error {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				seen[line] = true
			}
		}
		return nil
	}
	mergeBase, err := exec.Command("git", "merge-base", base, "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("no merge base with %s (git fetch origin?): %w", base, err)
	}
	// --no-renames lists both sides of a move: the source package loses a
	// file as surely as the destination gains one.
	if err := add("diff", "--name-only", "--no-renames", strings.TrimSpace(string(mergeBase))); err != nil { // committed + working tree
		return nil, err
	}
	if err := add("ls-files", "--others", "--exclude-standard"); err != nil {
		return nil, err
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// fullSuite names changes whose reach the gate does not compute: module
// requirements, the Raft replacement, the architecture ratchets, the
// composition roots and the definitions of the CI jobs themselves.
func fullSuite(f string) bool {
	switch {
	case f == "go.mod", f == "go.sum", f == "Makefile", f == "scripts/test-raft.sh",
		strings.HasPrefix(f, ".github/"), strings.HasPrefix(f, "third_party/"),
		strings.HasPrefix(f, "internal/architecture/"):
		return true
	case (strings.HasPrefix(f, "cmd/steve/") || strings.HasPrefix(f, "cmd/steve-node/")) &&
		path.Ext(f) == ".go" && !strings.HasSuffix(f, "_test.go"):
		return true
	}
	return false
}

// consoleFull names console inputs every script depends on.
func consoleFull(f string) bool {
	switch f {
	case "web/console/package.json", "web/console/package-lock.json", "web/console/vite.config.ts",
		"web/console/tsconfig.json", "web/console/tsconfig.app.json", "web/console/tsconfig.node.json":
		return true
	}
	return false
}

// dataReaders lists Go packages whose tests read repository files outside
// their own directory. Keep it in step with tests that open ../../<dir>.
var dataReaders = []struct {
	prefix string
	pkgs   []string
}{
	{"docs/protocol/", []string{"internal/clientapi"}},
	{"examples/", []string{"internal/app", "internal/contentreplica", "internal/node", "internal/plugins", "internal/plugins/pluginledger", "internal/runtime"}},
	{"plugins/", []string{"internal/plugins"}},
	{"web/console/src/lib/i18n/", []string{"internal/i18n"}},
}

// dataRead reports whether a Go test reads f, which then outranks inert.
func dataRead(f string) bool {
	for _, r := range dataReaders {
		if strings.HasPrefix(f, r.prefix) {
			return true
		}
	}
	return false
}

// consoleDist is the built console embedded into the binary. Only
// embedReaders serve it; the rest of the module just links it.
const consoleDist = "internal/readmodel/web/dist/"

var embedReaders = []string{"internal/readmodel/web", "internal/httpapi", "internal/cluster"}

// sourceReaders parse the module's Go source, so any Go change reaches them.
var sourceReaders = []string{"internal/architecture"}

// desktopInputs are what the desktop job (macOS) builds and runs.
func desktopInput(f string) bool {
	return strings.HasPrefix(f, "desktop/") || f == "scripts/test-desktop.sh" || f == "scripts/build-app-icon.sh"
}

// separateWorkflow paths have their own CI workflow outside the required checks.
func separateWorkflow(f string) string {
	for _, p := range []string{"providers/github/", "providers/mail/"} {
		if strings.HasPrefix(f, p) {
			return "npm test in " + strings.TrimSuffix(p, "/")
		}
	}
	return ""
}

// inert paths are read by no automated check that runs on a pull request,
// unless a package embeds them.
func inert(f string) bool {
	if !strings.Contains(f, "/") {
		return strings.HasSuffix(f, ".md") || strings.HasSuffix(f, ".example.json") ||
			f == "LICENSE" || f == ".gitignore" || f == ".goreleaser.yaml"
	}
	for _, p := range []string{"docs/", ".agents/", "e2e/ux/", "e2e/childcard/screenshots/"} {
		if strings.HasPrefix(f, p) {
			return true
		}
	}
	return f == "scripts/build-desktop.sh" || // release workflow only
		strings.HasSuffix(f, ".md") && !strings.HasPrefix(f, "plugins/") && !strings.HasPrefix(f, "examples/")
}
