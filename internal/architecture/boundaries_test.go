// Package architecture verifies dependency directions at the repository boundary.
package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every package under internal/ belongs to exactly one layer. Dependencies
// point inward only: entry → product → kernel. The kernel keeps the
// correctness machinery (ledger, execution recovery, stop confirmation,
// harness isolation, replay protection) and must not learn about the product
// it serves; the product layer (threads, tasks, approvals, progress) must not
// learn about the transports that expose it.
//
// A new package fails this test until it is placed in a layer.
var layers = map[string][]string{
	"kernel": {
		"ability", "acceptloop", "acphost", "adapter", "agent", "agentmcp", "agenttools", "approval",
		"artifact", "attempt", "budget", "capability", "channel", "checkpoint", "clientcap", "config",
		"configbuild", "contentreplica", "datalevel", "durable", "eventlog", "events", "execution",
		"filedoc", "fsx", "harness", "home", "httpdrain", "i18n", "identity", "idle", "ingress", "intent",
		"interaction", "launchconfig", "ledger", "lifecycle", "localtoken", "logs", "material",
		"mcpprobe", "mcpscan", "memory", "models", "nativehistory", "node", "nodewire", "peerstop",
		"permission", "pihome", "plugins", "processrestart", "procgroup", "project", "protocol",
		"provider", "roster", "runtime", "sameorigin", "schedule", "skills", "stableport", "state",
		"task", "text", "view", "workspacepath",
	},
	"product": {"agentexec", "approvals", "feed", "work"},
	"entry":   {},
}

var rank = map[string]int{"kernel": 0, "product": 1, "entry": 2}

func TestDependenciesPointInward(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "../.."))
	layerOf := map[string]string{"architecture": "entry"}
	for layer, packages := range layers {
		for _, p := range packages {
			if previous, ok := layerOf[p]; ok {
				t.Fatalf("%s is in both %s and %s", p, previous, layer)
			}
			layerOf[p] = layer
		}
	}
	unplaced := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == "testdata" {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(root, "internal"), file)
		owner := strings.Split(filepath.ToSlash(rel), "/")[0]
		ownerLayer, ok := layerOf[owner]
		if !ok {
			unplaced[owner] = true
			return nil
		}
		ast, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range ast.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			dependency, ok := strings.CutPrefix(importPath, "github.com/gopact-ai/steve/internal/")
			if !ok {
				continue
			}
			top := strings.Split(dependency, "/")[0]
			dependencyLayer, ok := layerOf[top]
			if !ok {
				unplaced[top] = true
				continue
			}
			if rank[dependencyLayer] > rank[ownerLayer] {
				t.Errorf("%s (%s) imports %s (%s): dependencies must point toward the kernel", rel, ownerLayer, dependency, dependencyLayer)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unplaced) > 0 {
		names := make([]string, 0, len(unplaced))
		for name := range unplaced {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Errorf("packages without a layer (add them to layers in boundaries_test.go): %s", strings.Join(names, ", "))
	}
}
