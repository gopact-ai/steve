// Package architecture verifies dependency directions at the repository boundary.
package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDependenciesRespectDomainAndTransportBoundaries(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "../.."))
	domains := map[string]bool{"ledger": true, "task": true, "project": true, "attempt": true, "ability": true, "capability": true, "agent": true, "state": true}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(root, "internal"), file)
		owner := strings.Split(filepath.ToSlash(rel), "/")[0]
		ast, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range ast.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			if !strings.HasPrefix(importPath, "github.com/gopact-ai/steve/internal/") {
				continue
			}
			dependency := strings.TrimPrefix(importPath, "github.com/gopact-ai/steve/internal/")
			top := strings.Split(dependency, "/")[0]
			if top == "httpapi" && owner != "httpapi" {
				t.Errorf("%s: implementation imports HTTP transport %s", rel, dependency)
			}
			if owner == "consoleapi" && (top == "httpapi" || top == "readmodel" || top == "console" || top == "gateway" || top == "turn") {
				t.Errorf("%s: API contract imports implementation %s", rel, dependency)
			}
			if domains[owner] && (top == "app" || top == "admin" || top == "cluster" || top == "httpapi" || top == "consoleapi" || top == "readmodel" || top == "console" || top == "gateway" || top == "turn" || top == "delegate" || top == "exec") {
				t.Errorf("%s: domain imports application/transport %s", rel, dependency)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
