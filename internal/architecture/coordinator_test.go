package architecture

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"unicode"
	"unicode/utf8"
)

// coordinatorSurface is how much of turn.Coordinator a reader has to hold
// in mind: the exported methods callers can reach (including those promoted
// from coordinatorState), the fields of coordinatorState, and the nil checks
// on those fields. requiredNilChecks are the checks on fields Deps.required
// lists, which New never leaves nil; nilChecks are those on every other
// field.
type coordinatorSurface struct {
	methods, fields, nilChecks, requiredNilChecks int
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
// A field is required when Deps.required lists it. The method's body must
// be a single `return []dependency{...}` whose entries are all positional,
// as in {"Name", absent}: the leading string is a Deps field name, and the
// coordinator field it fills is that name lower-cased. A list that is
// missing, written any other way, empty, or names no field of Coordinator or
// coordinatorState is an error.
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
	// members are the fields of Coordinator and coordinatorState.
	members := map[string]bool{}
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
				if spec.Name.Name == "Coordinator" {
					for _, name := range field.Names {
						members[name.Name] = true
					}
				}
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
						members[name] = true
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
	required, err := requiredFields(parsed)
	if err != nil {
		return coordinatorSurface{}, err
	}
	for field := range required {
		if !members[field] {
			return coordinatorSurface{}, fmt.Errorf("method Deps.required lists a dependency that fills no coordinator field %s", field)
		}
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
			// dependency is the field expr reads when it is x.f for a
			// dependency f, or "".
			dependency := func(expr ast.Expr) string {
				selector, ok := expr.(*ast.SelectorExpr)
				if !ok || !dependencies[selector.Sel.Name] {
					return ""
				}
				if root, ok := selector.X.(*ast.Ident); (ok && roots[root.Name]) || held(selector.X) {
					return selector.Sel.Name
				}
				return ""
			}
			// aliases are locals declared from a dependency, as in
			// `if x := recv.f; x == nil`, by the field they hold.
			aliases := map[string]string{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != len(assign.Rhs) {
					return true
				}
				for i, value := range assign.Rhs {
					if local, ok := assign.Lhs[i].(*ast.Ident); ok && dependency(value) != "" {
						aliases[local.Name] = dependency(value)
					}
				}
				return true
			})
			// checked is the field expr reads, directly or through an
			// alias, or "".
			checked := func(expr ast.Expr) string {
				if field := dependency(expr); field != "" {
					return field
				}
				if ident, ok := expr.(*ast.Ident); ok {
					return aliases[ident.Name]
				}
				return ""
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
				field := ""
				switch {
				case isNil(bin.Y):
					field = checked(bin.X)
				case isNil(bin.X):
					field = checked(bin.Y)
				}
				switch {
				case field == "":
				case required[field]:
					surface.requiredNilChecks++
				default:
					surface.nilChecks++
				}
				return true
			})
		}
	}
	return surface, nil
}

// requiredFields reads Deps.required: the coordinator field each entry's
// leading string names, lower-cased. Any other shape of the method is an
// error, so no entry is skipped unread.
func requiredFields(parsed []*ast.File) (map[string]bool, error) {
	var fn *ast.FuncDecl
	for _, syntax := range parsed {
		for _, decl := range syntax.Decls {
			if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == "required" && f.Recv != nil && len(f.Recv.List) > 0 && typeName(f.Recv.List[0].Type) == "Deps" && f.Body != nil {
				fn = f
			}
		}
	}
	if fn == nil {
		return nil, errors.New("no method Deps.required: the ratchet cannot tell required dependencies from optional ones")
	}
	var list *ast.CompositeLit
	if len(fn.Body.List) == 1 {
		if ret, ok := fn.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			list, _ = ret.Results[0].(*ast.CompositeLit)
		}
	}
	if list == nil {
		return nil, errors.New("method Deps.required is not a single return of a []dependency literal")
	}
	fields := map[string]bool{}
	for i, elt := range list.Elts {
		entry, ok := elt.(*ast.CompositeLit)
		if !ok || len(entry.Elts) == 0 {
			return nil, fmt.Errorf("method Deps.required: entry %d is not a positional {\"Name\", absent} literal", i+1)
		}
		lit, ok := entry.Elts[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return nil, fmt.Errorf("method Deps.required: entry %d does not open with the field name as a string", i+1)
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil || name == "" {
			return nil, fmt.Errorf("method Deps.required: entry %d names no field", i+1)
		}
		first, size := utf8.DecodeRuneInString(name)
		fields[string(unicode.ToLower(first))+name[size:]] = true
	}
	if len(fields) == 0 {
		return nil, errors.New("method Deps.required lists no dependency")
	}
	return fields, nil
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
	want := coordinatorSurface{methods: 3, fields: 6, nilChecks: 6, requiredNilChecks: 4}
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

// The required list is read from Deps.required, so it cannot drift from
// what New refuses; a list that is gone, written in a shape the reader
// would skip entries of, or names no coordinator field must stop the
// measurement rather than count a required field as optional.
func TestCoordinatorSurfaceRefusesAnUnusableRequiredList(t *testing.T) {
	const types = "package turn\n\ntype Coordinator struct{ *coordinatorState }\n\ntype coordinatorState struct{ tasks *int }\n\ntype Deps struct{ Tasks, Plans *int }\n\ntype dependency struct {\n\tname   string\n\tabsent bool\n}\n"
	for name, source := range map[string]string{
		"no list":       types,
		"empty list":    types + "\nfunc (d Deps) required() []dependency { return nil }\n",
		"unknown field": types + "\nfunc (d Deps) required() []dependency { return []dependency{{\"Plans\", d.Plans == nil}} }\n",
		"keyed entry":   types + "\nfunc (d Deps) required() []dependency { return []dependency{{\"Tasks\", d.Tasks == nil}, {name: \"Plans\", absent: d.Plans == nil}} }\n",
		"helper entry":  types + "\nfunc need(n string, a bool) dependency { return dependency{n, a} }\n\nfunc (d Deps) required() []dependency { return []dependency{{\"Tasks\", d.Tasks == nil}, need(\"Plans\", d.Plans == nil)} }\n",
		"built list":    types + "\nfunc (d Deps) required() []dependency {\n\tlist := []dependency{{\"Tasks\", d.Tasks == nil}}\n\treturn append(list, dependency{\"Plans\", d.Plans == nil})\n}\n",
	} {
		file := filepath.Join(t.TempDir(), "coordinator.go")
		if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		if surface, err := measureCoordinator([]string{file}); err == nil {
			t.Errorf("%s: measured %+v", name, surface)
		}
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
	if surface.requiredNilChecks != 0 {
		t.Errorf("internal/turn: %d nil checks on dependencies Deps.required lists; New never leaves those nil, so drop the checks", surface.requiredNilChecks)
	}
	ratchetWith(t, "coordinator_surface", []string{
		fmt.Sprintf("internal/turn.Coordinator exported methods %d", surface.methods),
		fmt.Sprintf("internal/turn.coordinatorState fields %d", surface.fields),
		fmt.Sprintf("internal/turn.coordinatorState nil optional dependency checks %d", surface.nilChecks),
	}, func(string) string { return "keep the entry and lower its number instead" })
}
