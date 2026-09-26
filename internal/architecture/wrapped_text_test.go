package architecture

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCatalogErrorsAreMadeByErrorf keeps an error said through the catalog
// findable with errors.Is. A template that wraps an error with %w only
// wraps it when fmt.Errorf formats it; Catalog.T formats with Sprintf,
// which leaves "%!w(...)" in the sentence and drops the error. So an error
// is made from a template by Catalog.Errorf, and a template that wraps an
// error is never read through Catalog.T.
func TestCatalogErrorsAreMadeByErrorf(t *testing.T) {
	root := repoRoot(t)
	wrapping := wrappingKeys(t, root)
	if len(wrapping) == 0 {
		t.Fatal("no catalog template wraps an error; the check below would pass whatever the code does")
	}
	files := sourceFiles(t, root)
	for _, source := range parseGoSources(t, root, files) {
		inCatalog := source.dir == "internal/i18n"
		// A template fmt.Errorf formats is reported once, as that call.
		reported := map[ast.Node]bool{}
		ast.Inspect(source.syntax, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || reported[call] {
				return true
			}
			site := source.rel + "#" + enclosing(source.syntax, call)
			if isCall(call, "fmt", "Errorf") && len(call.Args) > 0 {
				if inner, ok := call.Args[0].(*ast.CallExpr); ok && catalogKey(inner, inCatalog) != "" {
					t.Errorf("%s: fmt.Errorf formats the template of %s; make the error with Catalog.Errorf", site, catalogKey(inner, inCatalog))
					reported[inner] = true
					return true
				}
			}
			if key := catalogKey(call, inCatalog); key != "" && wrapping[key] {
				t.Errorf("%s: Catalog.T reads %s, which wraps an error; make the error with Catalog.Errorf", site, key)
			}
			return true
		})
	}
}

// wrappingKeys names the catalog keys whose template, in any language,
// wraps an error with %w.
func wrappingKeys(t *testing.T, root string) map[string]bool {
	t.Helper()
	sources := parseGoSources(t, root, []string{filepath.Join(root, "internal/i18n/catalog.go")})
	keys := map[string]bool{}
	ast.Inspect(sources[0].syntax, func(n ast.Node) bool {
		entry, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		name, ok := entry.Key.(*ast.Ident)
		if !ok {
			return true
		}
		ast.Inspect(entry.Value, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(strings.ReplaceAll(value, "%%", ""), "%w") {
					keys[name.Name] = true
				}
			}
			return true
		})
		return false
	})
	return keys
}

// isCall reports whether call is pkg.name(...).
func isCall(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// catalogKey is the key name of a call x.T(i18n.Key, ...), or of
// x.T(Key, ...) inside the catalog's own package; "" for any other call.
func catalogKey(call *ast.CallExpr, inCatalog bool) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" || len(call.Args) == 0 {
		return ""
	}
	switch key := call.Args[0].(type) {
	case *ast.SelectorExpr:
		if pkg, ok := key.X.(*ast.Ident); ok && pkg.Name == "i18n" {
			return key.Sel.Name
		}
	case *ast.Ident:
		if inCatalog {
			return key.Name
		}
	}
	return ""
}

// enclosing names the function declaration holding node.
func enclosing(file *ast.File, node ast.Node) string {
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Pos() <= node.Pos() && node.End() <= fd.End() {
			return declName(fd)
		}
	}
	return fmt.Sprint(node.Pos())
}
