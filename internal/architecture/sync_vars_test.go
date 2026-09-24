package architecture

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// packageSyncVars lists, as "dir.name", the package-level variables in
// files whose type or initial value comes from sync or sync/atomic: a
// mutex, once, map, pool, wait group, condition or atomic value, a pointer
// to one, a struct that embeds or holds one, or a named type of the same
// package, declared in any of the files given, that holds one. Function
// types are not looked into, nor are function literals in an initial
// value.
//
// The analysis is syntactic: it does not see such a value behind a named
// type of another package, or one returned by a function of another
// package. A file that dot-imports sync or sync/atomic names those types
// without the package, so it is returned in dotImports, as its path from
// root, instead of being analysed for them.
func packageSyncVars(t *testing.T, root string, files []string) (found, dotImports []string) {
	t.Helper()
	type file struct {
		src     goSource
		aliases map[string]bool
	}
	var parsed []file
	for _, src := range parseGoSources(t, root, files) {
		aliases := map[string]bool{}
		for _, spec := range src.syntax.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			if importPath != "sync" && importPath != "sync/atomic" {
				continue
			}
			name := importPath[strings.LastIndexByte(importPath, '/')+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if name == "." {
				dotImports = append(dotImports, src.rel)
				continue
			}
			aliases[name] = true
		}
		parsed = append(parsed, file{src: src, aliases: aliases})
	}
	// synced holds, per directory, the named types found to hold
	// synchronization state.
	synced := map[string]map[string]bool{}
	fromSync := func(f file, n ast.Node) bool {
		if n == nil {
			return false
		}
		local := synced[f.src.dir]
		hit := false
		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncType, *ast.FuncLit:
				return false
			case *ast.Field:
				// A field's name is not a type.
				ast.Inspect(n.Type, visit)
				return false
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && f.aliases[x.Name] {
					hit = true
				}
				return false
			case *ast.Ident:
				if local[n.Name] {
					hit = true
				}
			}
			return !hit
		}
		ast.Inspect(n, visit)
		return hit
	}
	for changed := true; changed; {
		changed = false
		for _, f := range parsed {
			for _, decl := range f.src.syntax.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					typ := spec.(*ast.TypeSpec)
					if synced[f.src.dir][typ.Name.Name] || !fromSync(f, typ.Type) {
						continue
					}
					if synced[f.src.dir] == nil {
						synced[f.src.dir] = map[string]bool{}
					}
					synced[f.src.dir][typ.Name.Name] = true
					changed = true
				}
			}
		}
	}
	for _, f := range parsed {
		if len(f.aliases) == 0 && len(synced[f.src.dir]) == 0 {
			continue
		}
		initialValue := func(value ast.Expr) bool {
			for {
				switch v := value.(type) {
				case *ast.ParenExpr:
					value = v.X
					continue
				case *ast.UnaryExpr:
					value = v.X
					continue
				case *ast.CompositeLit:
					return fromSync(f, v.Type)
				case *ast.CallExpr:
					if ident, ok := v.Fun.(*ast.Ident); ok && ident.Name == "new" && len(v.Args) == 1 {
						return fromSync(f, v.Args[0])
					}
					if selector, ok := v.Fun.(*ast.SelectorExpr); ok {
						return fromSync(f, selector)
					}
				}
				return false
			}
		}
		for _, decl := range f.src.syntax.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				typed := fromSync(f, value.Type)
				for i, name := range value.Names {
					if name.Name == "_" {
						continue
					}
					if typed || i < len(value.Values) && initialValue(value.Values[i]) {
						found = append(found, f.src.dir+"."+name.Name)
					}
				}
			}
		}
	}
	sort.Strings(found)
	sort.Strings(dotImports)
	return found, dotImports
}

func TestPackageSyncVarsFindsSynchronizationState(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal", "architecture", "testdata", "package_sync_vars")
	got, dotted := packageSyncVars(t, root, []string{
		filepath.Join(root, "internal", "dotted", "dotted.go"),
		filepath.Join(root, "internal", "sample", "other.go"),
		filepath.Join(root, "internal", "sample", "sample.go"),
	})
	want := []string{
		"internal/sample.allocated", "internal/sample.boxed", "internal/sample.counter", "internal/sample.deep",
		"internal/sample.elsewhere", "internal/sample.guarded", "internal/sample.held", "internal/sample.literal",
		"internal/sample.made", "internal/sample.mu", "internal/sample.once", "internal/sample.pointer",
		"internal/sample.rw", "internal/sample.shared", "internal/sample.value",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("package sync vars =\n%v\nwant\n%v", got, want)
	}
	if want := []string{"internal/dotted/dotted.go"}; fmt.Sprint(dotted) != fmt.Sprint(want) {
		t.Fatalf("dot imports = %v, want %v", dotted, want)
	}
}

// A package-level mutex or atomic is shared by every instance in the
// process, so two applications, peers or services built in one process
// read and write the same state. Such state belongs to the instance that
// uses it; testdata/package_sync_vars.txt lists the variables that guard
// what the process itself has only one of, each followed by why.
func TestPackageSyncVarsAreAllowed(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "internal", "architecture", "testdata", "package_sync_vars.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		key, reason, _ := strings.Cut(line, " ")
		if reason = strings.TrimSpace(reason); reason == "" {
			t.Errorf("package_sync_vars: %s does not say why it is process-wide", key)
		}
		allowed[key] = reason
	}
	seen := map[string]bool{}
	found, dotImports := packageSyncVars(t, root, sourceFiles(t, root))
	for _, file := range dotImports {
		t.Errorf("package_sync_vars: %s dot-imports sync or sync/atomic, which hides synchronization types from this check; import the package by name", file)
	}
	for _, name := range found {
		seen[name] = true
		if _, ok := allowed[name]; !ok {
			t.Errorf("package_sync_vars: new package-level synchronization variable %s — hold it in the instance that uses it, or, when it guards state the process has only one of, list it in testdata/package_sync_vars.txt with the reason", name)
		}
	}
	for key := range allowed {
		if !seen[key] {
			t.Errorf("package_sync_vars: %s is no longer a package-level synchronization variable; remove it from testdata/package_sync_vars.txt", key)
		}
	}
}
