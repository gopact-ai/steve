package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// openCapabilities are interfaces that several production types satisfy or
// lack on purpose, so probing for them is the design rather than a hidden
// dependency. Each value says why.
var openCapabilities = map[string]string{
	"internal/lifecycle.Stopper":       "only some processes can be stopped gracefully",
	"internal/memory.namedSource":      "memory sources differ in whether they have a name",
	"internal/memory.textReader":       "memory sources differ in whether they have readable text",
	"internal/nodewire.writeDeadliner": "connection types differ in write deadline support",
	"internal/node.halfCloser":         "connection types differ in half-close support",
	"internal/ledger.backupSource":     "only SQLite driver connections expose backup",
	"internal/ledger.restoreTarget":    "only SQLite driver connections expose restore",
}

// goSource is one parsed Go file, with its directory relative to the root.
type goSource struct {
	rel, dir string
	syntax   *ast.File
}

func parseGoSources(t *testing.T, root string, files []string) []goSource {
	t.Helper()
	parsed := make([]goSource, 0, len(files))
	for _, file := range files {
		syntax, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, file)
		rel = filepath.ToSlash(rel)
		parsed = append(parsed, goSource{rel: rel, dir: path.Dir(rel), syntax: syntax})
	}
	return parsed
}

// unpinnedProbes maps each probed interface that nothing pins to the files
// that probe it.
//
// A probe is a type assertion or type switch case whose target is a
// non-empty interface declared in a non-test file of this repository. A pin
// is a package-level `var _ I = (*T)(nil)`, `var _ I = T{}` or
// `var _ I = pkg.T{}` in a _test.go file, with T declared in a non-test file;
// it fails to compile once that production type stops satisfying I.
// Interfaces in open are neither required nor expected to be pinned.
//
// The analysis is syntactic. It resolves an identifier to the file's own
// package and a selector to an import of this module, so it does not see
// instantiated generic interfaces (I[T]), interfaces named through a type
// alias or a dot import, or a local type that shadows a package-level one.
func unpinnedProbes(t *testing.T, root string, sources, tests []string, open map[string]string) map[string][]string {
	t.Helper()
	parsedSources := parseGoSources(t, root, sources)
	packages := map[string]string{}
	interfaces := map[string]bool{}
	declared := map[string]bool{}
	for _, src := range parsedSources {
		packages[src.dir] = src.syntax.Name.Name
		for _, decl := range src.syntax.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				declared[src.dir+"."+typeSpec.Name.Name] = true
				if iface, ok := typeSpec.Type.(*ast.InterfaceType); ok && typeSpec.Assign == 0 && len(iface.Methods.List) > 0 {
					interfaces[src.dir+"."+typeSpec.Name.Name] = true
				}
			}
		}
	}
	openNames := make([]string, 0, len(open))
	for name := range open {
		openNames = append(openNames, name)
	}
	sort.Strings(openNames)
	for _, name := range openNames {
		switch {
		case !interfaces[name]:
			t.Errorf("openCapabilities lists %s, which is not a non-empty interface declared in this repository; remove it", name)
		case strings.TrimSpace(open[name]) == "":
			t.Errorf("openCapabilities lists %s without saying why several production types satisfy or lack it", name)
		}
	}
	// resolve qualifies the type name an expression refers to, or returns "".
	resolve := func(src goSource) func(ast.Expr) string {
		imports := map[string]string{}
		for _, spec := range src.syntax.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			dir, ok := strings.CutPrefix(importPath, module)
			if !ok {
				continue
			}
			name, known := packages[dir]
			if !known {
				name = path.Base(dir)
			}
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = dir
		}
		// An external test package cannot name its package's identifiers
		// unqualified.
		ownPackage := packages[src.dir] == src.syntax.Name.Name
		return func(target ast.Expr) string {
			for {
				paren, ok := target.(*ast.ParenExpr)
				if !ok {
					break
				}
				target = paren.X
			}
			switch target := target.(type) {
			case *ast.Ident:
				if ownPackage {
					return src.dir + "." + target.Name
				}
			case *ast.SelectorExpr:
				if pkg, ok := target.X.(*ast.Ident); ok && imports[pkg.Name] != "" {
					return imports[pkg.Name] + "." + target.Sel.Name
				}
			}
			return ""
		}
	}
	// pins reports whether a pin's value is (*T)(nil), T{} or pkg.T{} with T
	// declared in a non-test file, so a test fake or a bare nil pins nothing.
	pins := func(name func(ast.Expr) string, value ast.Expr) bool {
		switch value := value.(type) {
		case *ast.CompositeLit:
			return declared[name(value.Type)]
		case *ast.CallExpr:
			if len(value.Args) != 1 {
				return false
			}
			arg, isIdent := value.Args[0].(*ast.Ident)
			conversion, isParen := value.Fun.(*ast.ParenExpr)
			if !isIdent || arg.Name != "nil" || !isParen {
				return false
			}
			pointer, isPointer := conversion.X.(*ast.StarExpr)
			return isPointer && declared[name(pointer.X)]
		}
		return false
	}
	probes := map[string]map[string]bool{}
	for _, src := range parsedSources {
		name := resolve(src)
		probe := func(target ast.Expr) {
			if iface := name(target); interfaces[iface] {
				if probes[iface] == nil {
					probes[iface] = map[string]bool{}
				}
				probes[iface][src.rel] = true
			}
		}
		ast.Inspect(src.syntax, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeAssertExpr:
				if n.Type != nil {
					probe(n.Type)
				}
			case *ast.TypeSwitchStmt:
				for _, clause := range n.Body.List {
					for _, target := range clause.(*ast.CaseClause).List {
						probe(target)
					}
				}
			}
			return true
		})
	}
	for _, src := range parseGoSources(t, root, tests) {
		name := resolve(src)
		for _, decl := range src.syntax.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				if len(value.Names) == 1 && value.Names[0].Name == "_" && value.Type != nil && len(value.Values) == 1 && pins(name, value.Values[0]) {
					delete(probes, name(value.Type))
				}
			}
		}
	}
	unpinned := map[string][]string{}
	for iface, files := range probes {
		if _, ok := open[iface]; ok {
			continue
		}
		for file := range files {
			unpinned[iface] = append(unpinned[iface], file)
		}
		sort.Strings(unpinned[iface])
	}
	return unpinned
}

func TestUnpinnedProbesExcludePinnedAndOpenInterfaces(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal", "architecture", "testdata", "named_assertions")
	fixture := func(parts ...string) string { return filepath.Join(append([]string{root, "internal"}, parts...)...) }
	sources := []string{fixture("port", "port.go"), fixture("user", "user.go")}
	tests := []string{fixture("port", "external_test.go"), fixture("user", "contracts_test.go")}
	open := map[string]string{"internal/user.reader": "fixture"}
	got := unpinnedProbes(t, root, sources, tests, open)
	want := map[string][]string{
		"internal/port.Faked":  {"internal/user/user.go"},
		"internal/port.Nilled": {"internal/user/user.go"},
		"internal/port.local":  {"internal/port/port.go"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("unpinned probes = %v, want %v", got, want)
	}
}

// A probe for an interface with one production implementation compiles on
// and silently takes its fallback once that implementation stops matching.
// Its pin fails to compile instead.
func TestUnpinnedInterfaceProbesOnlyShrink(t *testing.T) {
	root := repoRoot(t)
	unpinned := unpinnedProbes(t, root, sourceFiles(t, root), testFiles(t, root), openCapabilities)
	current := make([]string, 0, len(unpinned))
	for iface := range unpinned {
		current = append(current, iface)
	}
	ratchetWith(t, "unpinned_interface_probes", current, func(iface string) string {
		return fmt.Sprintf("probed in %s. If one production type implements it, pin that type with `var _ %s = …` in a contracts_test.go of a package that can name both. If several production types satisfy or lack it on purpose, add it to openCapabilities with the reason.",
			strings.Join(unpinned[iface], ", "), path.Base(iface))
	})
}
