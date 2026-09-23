package architecture

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/gopact-ai/steve/"

// runLayer is where Steve executes work; storeLayer is where it keeps it.
// A denied path covers its subpackages too.
var (
	runLayer   = []string{"internal/exec", "internal/lifecycle", "internal/turn", "internal/node", "internal/harness", "internal/artifact"}
	storeLayer = []string{"internal/ledger", "modernc.org/sqlite"}
)

// reachRules bound what a package may link at all, directly or through any
// chain of imports. Paths without a dot are relative to the module.
var reachRules = []struct {
	from   string
	denied []string
}{
	// The file format and the text catalog describe, they do not run.
	{"internal/config", slices.Concat(runLayer, storeLayer)},
	{"internal/i18n", slices.Concat(runLayer, storeLayer)},
	// The console contract carries task, project, material and
	// plugin-library records, which live beside their ledger stores, and
	// artifact edits.
	{"internal/consoleapi", slices.DeleteFunc(slices.Clone(runLayer), func(pkg string) bool { return pkg == "internal/artifact" })},
	// A node keeps its own state; the hub's ledger is not its business.
	{"cmd/steve-node", []string{"internal/ledger"}},
}

// sqliteImporters are the only packages steve-node may reach SQLite
// through: the node's own session records.
var sqliteImporters = []string{"internal/node"}

// platforms are the targets Steve ships for; build tags change the graph.
var platforms = []string{"linux", "darwin", "windows"}

func TestPackagesDoNotReachDeniedLayers(t *testing.T) {
	for _, rule := range reachRules {
		for _, denied := range rule.denied {
			requireExists(t, denied)
		}
		for _, goos := range platforms {
			graph := importGraph(t, goos, rule.from)
			for _, denied := range rule.denied {
				if chain := importChain(graph, qualify(rule.from), qualify(denied)); chain != nil {
					t.Errorf("%s must not reach %s on %s: %s", rule.from, denied, goos, render(chain))
				}
			}
		}
	}
}

func TestNodeBinaryReachesSQLiteOnlyForSessionRecords(t *testing.T) {
	for _, goos := range platforms {
		graph := importGraph(t, goos, "cmd/steve-node")
		for _, pkg := range slices.Sorted(maps.Keys(graph)) {
			if slices.Contains(graph[pkg], "modernc.org/sqlite") && !slices.Contains(sqliteImporters, strings.TrimPrefix(pkg, module)) {
				chain := importChain(graph, qualify("cmd/steve-node"), pkg)
				t.Errorf("cmd/steve-node reaches SQLite outside the node's session records on %s: %s -> modernc.org/sqlite", goos, render(chain))
			}
		}
	}
}

// importGraph maps every package pkg links on goos, itself included, to its
// non-test imports.
func importGraph(t *testing.T, goos, pkg string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", "./"+pkg)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "GOOS="+goos)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", pkg, err, stderr(err))
	}
	graph := map[string][]string{}
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			graph[fields[0]] = fields[1:]
		}
	}
	return graph
}

// requireExists fails a rule that names a package no longer there, which
// would otherwise hold forever without checking anything.
func requireExists(t *testing.T, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "list", "-e", "-f", "{{if .Error}}{{.Error}}{{end}}", qualify(pkg))
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("rule names %s, which does not build: %v %s%s", pkg, err, out, stderr(err))
	}
}

func stderr(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(exit.Stderr)
	}
	return ""
}

// importChain is the shortest import path from one package to another, or
// nil if there is none.
func importChain(graph map[string][]string, from, to string) []string {
	parent := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if pkg == to || strings.HasPrefix(pkg, to+"/") {
			var chain []string
			for ; pkg != ""; pkg = parent[pkg] {
				chain = append(chain, pkg)
			}
			slices.Reverse(chain)
			return chain
		}
		for _, next := range graph[pkg] {
			if _, seen := parent[next]; !seen {
				parent[next] = pkg
				queue = append(queue, next)
			}
		}
	}
	return nil
}

func qualify(pkg string) string {
	if first, _, _ := strings.Cut(pkg, "/"); strings.Contains(first, ".") {
		return pkg
	}
	return module + pkg
}

func render(chain []string) string {
	short := make([]string, len(chain))
	for i, pkg := range chain {
		short[i] = strings.TrimPrefix(pkg, module)
	}
	return strings.Join(short, " -> ")
}
