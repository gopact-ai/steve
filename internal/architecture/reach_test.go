package architecture

import (
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/gopact-ai/steve/"

// runLayer is where Steve executes work; storeLayer is where it keeps it.
var (
	runLayer   = []string{"internal/exec", "internal/lifecycle", "internal/turn", "internal/node", "internal/harness"}
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
	// The console contract still carries task, project, material and
	// plugin-library records, which live beside their ledger stores.
	{"internal/consoleapi", runLayer},
	// A node keeps its own state; the hub's ledger is not its business.
	{"cmd/steve-node", []string{"internal/ledger"}},
}

// sqliteImporters are the only packages steve-node may reach SQLite
// through: the node's own session records.
var sqliteImporters = []string{"internal/node"}

func TestPackagesDoNotReachDeniedLayers(t *testing.T) {
	for _, rule := range reachRules {
		graph := importGraph(t, rule.from)
		for _, denied := range rule.denied {
			if chain := importChain(graph, qualify(rule.from), qualify(denied)); chain != nil {
				t.Errorf("%s must not reach %s: %s", rule.from, denied, render(chain))
			}
		}
	}
}

func TestNodeBinaryReachesSQLiteOnlyForSessionRecords(t *testing.T) {
	graph := importGraph(t, "cmd/steve-node")
	for _, pkg := range slices.Sorted(maps.Keys(graph)) {
		if slices.Contains(graph[pkg], "modernc.org/sqlite") && !slices.Contains(sqliteImporters, strings.TrimPrefix(pkg, module)) {
			chain := importChain(graph, qualify("cmd/steve-node"), pkg)
			t.Errorf("cmd/steve-node reaches SQLite outside the node's session records: %s -> modernc.org/sqlite", render(chain))
		}
	}
}

// importGraph maps every package pkg links, itself included, to its
// non-test imports.
func importGraph(t *testing.T, pkg string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", "./"+pkg)
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
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

// importChain is the shortest import path from one package to another, or
// nil if there is none.
func importChain(graph map[string][]string, from, to string) []string {
	parent := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if pkg == to {
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
