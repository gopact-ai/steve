package architecture

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// coordinatorSurface is how much of turn.Coordinator a reader has to hold
// in mind: the exported methods callers can reach (including those promoted
// from coordinatorState), the fields of coordinatorState, and the nil checks
// on those fields that make a dependency look optional.
type coordinatorSurface struct {
	methods, fields, nilChecks int
}

// measureCoordinator reads the surface from the given non-test files of one
// package. A nil check is `x.f == nil` or `x.f != nil` where f is a field of
// coordinatorState that is not a map (maps are runtime tables created
// lazily, not dependencies) and x is, inside a method:
//   - the receiver of a method on Coordinator, coordinatorState or a struct
//     that embeds *Coordinator;
//   - recv.h, where h is a *Coordinator field of the receiver's struct;
//   - a local declared from recv.h, as in `x := recv.h` or `x, y := recv.h, v`.
//
// A local declared from such an x.f, as in `if d := x.f; d == nil`, is
// checked the same way. An embedded field counts under its type's name.
//
// The analysis is syntactic: a coordinator reached any other way is missed.
func measureCoordinator(files []string) (coordinatorSurface, error) {
	parsed := make([]*ast.File, 0, len(files))
	for _, file := range files {
		syntax, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			return coordinatorSurface{}, err
		}
		parsed = append(parsed, syntax)
	}
	var surface coordinatorSurface
	dependencies := map[string]bool{}
	// views embed *Coordinator, so their receivers reach the same fields;
	// holders name the fields through which other structs hold one.
	views := map[string]bool{}
	holders := map[string]map[string]bool{}
	declared := map[string]bool{}
	for _, syntax := range parsed {
		ast.Inspect(syntax, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			fields, ok := spec.Type.(*ast.StructType)
			if !ok {
				return false
			}
			declared[spec.Name.Name] = true
			for _, field := range fields.Fields.List {
				if spec.Name.Name == "coordinatorState" {
					_, table := field.Type.(*ast.MapType)
					names := make([]string, 0, len(field.Names))
					for _, name := range field.Names {
						names = append(names, name.Name)
					}
					if len(field.Names) == 0 {
						names = append(names, embeddedName(field.Type))
					}
					for _, name := range names {
						surface.fields++
						if !table {
							dependencies[name] = true
						}
					}
					continue
				}
				if typeName(field.Type) != "Coordinator" {
					continue
				}
				if len(field.Names) == 0 {
					views[spec.Name.Name] = true
				}
				for _, name := range field.Names {
					if holders[spec.Name.Name] == nil {
						holders[spec.Name.Name] = map[string]bool{}
					}
					holders[spec.Name.Name][name.Name] = true
				}
			}
			return false
		})
	}
	// Without the types, every count would read zero and the ratchet would
	// pass whatever the package became.
	for _, name := range []string{"Coordinator", "coordinatorState"} {
		if !declared[name] {
			return coordinatorSurface{}, fmt.Errorf("no struct type %s: rename the ratchet along with it", name)
		}
	}
	if surface.fields == 0 {
		return coordinatorSurface{}, errors.New("coordinatorState has no fields: the ratchet no longer measures it")
	}
	for _, syntax := range parsed {
		for _, decl := range syntax.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || len(fn.Recv.List[0].Names) == 0 {
				continue
			}
			receiver, recvType := fn.Recv.List[0].Names[0].Name, typeName(fn.Recv.List[0].Type)
			own := recvType == "Coordinator" || recvType == "coordinatorState"
			if own && fn.Name.IsExported() {
				surface.methods++
			}
			if fn.Body == nil {
				continue
			}
			// held reports whether expr is recv.h for a *Coordinator field h.
			held := func(expr ast.Expr) bool {
				selector, ok := expr.(*ast.SelectorExpr)
				if !ok {
					return false
				}
				root, ok := selector.X.(*ast.Ident)
				return ok && root.Name == receiver && holders[recvType][selector.Sel.Name]
			}
			roots := map[string]bool{}
			if own || views[recvType] {
				roots[receiver] = true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != len(assign.Rhs) {
					return true
				}
				for i, value := range assign.Rhs {
					if local, ok := assign.Lhs[i].(*ast.Ident); ok && held(value) {
						roots[local.Name] = true
					}
				}
				return true
			})
			dependency := func(expr ast.Expr) bool {
				selector, ok := expr.(*ast.SelectorExpr)
				if !ok || !dependencies[selector.Sel.Name] {
					return false
				}
				root, ok := selector.X.(*ast.Ident)
				return (ok && roots[root.Name]) || held(selector.X)
			}
			// aliases are locals declared from a dependency, as in
			// `if x := recv.f; x == nil`.
			aliases := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != len(assign.Rhs) {
					return true
				}
				for i, value := range assign.Rhs {
					if local, ok := assign.Lhs[i].(*ast.Ident); ok && dependency(value) {
						aliases[local.Name] = true
					}
				}
				return true
			})
			checked := func(expr ast.Expr) bool {
				ident, ok := expr.(*ast.Ident)
				return dependency(expr) || (ok && aliases[ident.Name])
			}
			isNil := func(expr ast.Expr) bool {
				ident, ok := expr.(*ast.Ident)
				return ok && ident.Name == "nil"
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				bin, ok := n.(*ast.BinaryExpr)
				if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
					return true
				}
				if (checked(bin.X) && isNil(bin.Y)) || (isNil(bin.X) && checked(bin.Y)) {
					surface.nilChecks++
				}
				return true
			})
		}
	}
	return surface, nil
}

// embeddedName is the field name an embedded type is reached by.
func embeddedName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return embeddedName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.IndexExpr:
		return embeddedName(e.X)
	case *ast.IndexListExpr:
		return embeddedName(e.X)
	}
	return ""
}

// typeName is the name of a type or of the type a pointer points to, or ""
// for any other type expression.
func typeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func TestCoordinatorSurfaceCountsReceiverRootedNilChecks(t *testing.T) {
	file := filepath.Join(repoRoot(t), "internal", "architecture", "testdata", "coordinator_surface", "coordinator.go")
	got, err := measureCoordinator([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	want := coordinatorSurface{methods: 3, fields: 6, nilChecks: 10}
	if got != want {
		t.Fatalf("coordinator surface = %+v, want %+v", got, want)
	}
}

// A rename must not turn the ratchet into one that always passes: without
// the types it measures, every count would read as zero.
func TestCoordinatorSurfaceRefusesMissingTypes(t *testing.T) {
	renamed := filepath.Join(repoRoot(t), "internal", "architecture", "testdata", "coordinator_surface_renamed", "coordinator.go")
	if surface, err := measureCoordinator([]string{renamed}); err == nil {
		t.Errorf("measured %+v from a package without Coordinator and coordinatorState", surface)
	}
	emptied := filepath.Join(t.TempDir(), "coordinator.go")
	source := "package turn\n\ntype Coordinator struct{ *coordinatorState }\n\ntype coordinatorState struct{}\n"
	if err := os.WriteFile(emptied, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if surface, err := measureCoordinator([]string{emptied}); err == nil {
		t.Errorf("measured %+v from a coordinatorState with no fields", surface)
	}
}

// Every dependency the coordinator can be built without is a second way
// for it to run, reachable only from tests; every exported method and
// shared field widens what a change has to reason about. None may grow.
func TestCoordinatorSurfaceOnlyShrinks(t *testing.T) {
	root := repoRoot(t)
	var files []string
	for _, file := range sourceFiles(t, root) {
		if rel, _ := filepath.Rel(root, file); filepath.ToSlash(filepath.Dir(rel)) == "internal/turn" {
			files = append(files, file)
		}
	}
	surface, err := measureCoordinator(files)
	if err != nil {
		t.Fatal(err)
	}
	ratchetWith(t, "coordinator_surface", []string{
		fmt.Sprintf("internal/turn.Coordinator exported methods %d", surface.methods),
		fmt.Sprintf("internal/turn.coordinatorState fields %d", surface.fields),
		fmt.Sprintf("internal/turn.coordinatorState nil dependency checks %d", surface.nilChecks),
	}, func(string) string { return "keep the entry and lower its number instead" })
}
