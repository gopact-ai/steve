package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestEveryKeyIsTranslated guards a failure mode that ships silently: T falls
// back to the raw key when a catalog entry is missing, so a forgotten
// translation reaches the user as "tasks_empty" rather than as an error.
// Parsing the const block keeps this test correct as keys are added.
func TestEveryKeyIsTranslated(t *testing.T) {
	declared := declaredKeys(t)
	if len(declared) == 0 {
		t.Fatal("no keys parsed; the const block moved and this test is now blind")
	}
	for _, locale := range []struct {
		name    string
		catalog map[Key]string
	}{{"zh", zh}, {"en", en}} {
		for _, key := range declared {
			value, ok := locale.catalog[key]
			if !ok {
				t.Errorf("%s catalog is missing %q", locale.name, key)
				continue
			}
			if value == "" {
				t.Errorf("%s catalog has %q empty", locale.name, key)
			}
			if value == string(key) {
				t.Errorf("%s catalog leaves %q as its own key", locale.name, key)
			}
		}
		for key := range locale.catalog {
			if !contains(declared, key) {
				t.Errorf("%s catalog has %q, which is not a declared Key", locale.name, key)
			}
		}
	}
}

func declaredKeys(t *testing.T) []Key {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "i18n.go", nil, 0)
	if err != nil {
		t.Fatalf("parse i18n.go: %v", err)
	}
	var keys []Key
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		ident, ok := spec.Type.(*ast.Ident)
		if !ok || ident.Name != "Key" {
			return true
		}
		for _, value := range spec.Values {
			lit, ok := value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			keys = append(keys, Key(unquoted))
		}
		return true
	})
	return keys
}

func contains(keys []Key, want Key) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}
