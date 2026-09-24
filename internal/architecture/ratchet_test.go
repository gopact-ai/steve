package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The ratchets below only let the codebase improve: they fail when a new
// offender appears, when the baseline still lists one that is gone, and
// when a sized entry differs from its baseline. Regenerate a baseline after
// an improvement with RATCHET_UPDATE=1.

const (
	// longFunction is the length a non-test function may not exceed
	// unless the baseline already lists it.
	longFunction = 120
	// cmdSteveMaxLines bounds the composition root, which only shrinks.
	cmdSteveMaxLines = 670
	// cmdSteveMaxFanOut bounds how many internal packages cmd/steve wires
	// directly, which only shrinks.
	cmdSteveMaxFanOut = 15
)

// stateWords are the bare string states and actions that are still
// compared inline instead of through a typed constant.
var stateWords = map[string]bool{"open": true, "close": true, "prompt": true, "poll": true, "attach": true, "running": true, "done": true, "failed": true, "idle": true, "queued": true}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(here), "../.."))
}

// sourceFiles lists every non-test Go file under internal/ and cmd/.
func sourceFiles(t *testing.T, root string) []string {
	t.Helper()
	return goFiles(t, root, false)
}

// testFiles lists every _test.go file under internal/ and cmd/.
func testFiles(t *testing.T, root string) []string {
	t.Helper()
	return goFiles(t, root, true)
}

func goFiles(t *testing.T, root string, tests bool) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || entry.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && strings.HasSuffix(path, "_test.go") == tests {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(files)
	return files
}

func funcName(rel string, fd *ast.FuncDecl) string {
	pkg := filepath.ToSlash(filepath.Dir(rel))
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		typ := fd.Recv.List[0].Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		if ident, ok := typ.(*ast.Ident); ok {
			return pkg + "." + ident.Name + "." + fd.Name.Name
		}
	}
	return pkg + "." + fd.Name.Name
}

// ratchet compares the current offenders against a baseline file: nothing
// new may appear, and nothing gone may linger.
func ratchet(t *testing.T, name string, current []string) {
	t.Helper()
	ratchetWith(t, name, current, func(string) string { return "fix it rather than adding it to the baseline" })
}

// ratchetWith is ratchet with fix saying how to resolve a new offender.
func ratchetWith(t *testing.T, name string, current []string, fix func(item string) string) {
	t.Helper()
	sort.Strings(current)
	path := filepath.Join(repoRoot(t), "internal", "architecture", "testdata", name+".txt")
	if os.Getenv("RATCHET_UPDATE") == "1" {
		data := strings.Join(current, "\n")
		if len(current) > 0 {
			data += "\n"
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read baseline %s: %v (regenerate with RATCHET_UPDATE=1)", name, err)
	}
	for _, problem := range ratchetProblems(name, strings.Split(string(raw), "\n"), current, fix) {
		t.Error(problem)
	}
}

// ratchetProblems compares current against the baseline lines. An entry is
// "name" or "name size"; a sized entry must match its baseline, so a
// shrink is recorded before anything can grow back into it.
func ratchetProblems(name string, lines, current []string, fix func(item string) string) []string {
	split := func(entry string) (string, int) {
		if i := strings.LastIndexByte(entry, ' '); i >= 0 {
			if size, err := strconv.Atoi(entry[i+1:]); err == nil {
				return entry[:i], size
			}
		}
		return entry, 0
	}
	baseline := map[string]int{}
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			key, size := split(line)
			baseline[key] = size
		}
	}
	var problems []string
	seen := map[string]bool{}
	for _, item := range current {
		key, size := split(item)
		seen[key] = true
		limit, listed := baseline[key]
		switch {
		case !listed:
			problems = append(problems, fmt.Sprintf("%s: new offender %s — %s", name, item, fix(item)))
		case size > limit:
			problems = append(problems, fmt.Sprintf("%s: %s grew past its baseline (%d > %d)", name, key, size, limit))
		case size < limit:
			problems = append(problems, fmt.Sprintf("%s: %s shrank below its baseline (%d < %d); lower it in testdata/%s.txt", name, key, size, limit, name))
		}
	}
	keys := make([]string, 0, len(baseline))
	for key := range baseline {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] {
			problems = append(problems, fmt.Sprintf("%s: %s is no longer an offender; remove it from testdata/%s.txt", name, key, name))
		}
	}
	return problems
}

// A sized entry pins its size: growing past it is a regression, and
// shrinking below it leaves room for the next change to grow back unseen.
func TestRatchetPinsSizedEntries(t *testing.T) {
	fix := func(string) string { return "fix it" }
	baseline := []string{"a 10", "b 5", "gone"}
	for _, tc := range []struct {
		name    string
		current []string
		want    []string
	}{
		{"equal", []string{"a 10", "b 5", "gone"}, nil},
		{"grew", []string{"a 11", "b 5", "gone"}, []string{"r: a grew past its baseline (11 > 10)"}},
		{"shrank", []string{"a 9", "b 5", "gone"}, []string{"r: a shrank below its baseline (9 < 10); lower it in testdata/r.txt"}},
		{"new", []string{"a 10", "b 5", "gone", "c"}, []string{"r: new offender c — fix it"}},
		{"removed", []string{"a 10", "b 5"}, []string{"r: gone is no longer an offender; remove it from testdata/r.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ratchetProblems("r", baseline, tc.current, fix)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("problems = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLongFunctionsOnlyShrink(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	for _, file := range sourceFiles(t, root) {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, file)
		for _, decl := range parsed.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			lines := fset.Position(fd.End()).Line - fset.Position(fd.Pos()).Line + 1
			if lines > longFunction {
				offenders = append(offenders, fmt.Sprintf("%s %d", funcName(rel, fd), lines))
			}
		}
	}
	ratchet(t, "long_functions", offenders)
}

func TestInlineInterfaceAssertionsOnlyShrink(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	for _, file := range sourceFiles(t, root) {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, file)
		ast.Inspect(parsed, func(n ast.Node) bool {
			assertion, ok := n.(*ast.TypeAssertExpr)
			if !ok || assertion.Type == nil {
				return true
			}
			if _, inline := assertion.Type.(*ast.InterfaceType); inline {
				offenders = append(offenders, filepath.ToSlash(rel)+":"+strconv.Itoa(fset.Position(assertion.Pos()).Line))
			}
			return true
		})
	}
	ratchet(t, "interface_assertions", offenders)
}

// consoleapi.Admin, Console and PluginsService are each implemented by one
// service and consumed only by httpapi. A method httpapi neither calls nor
// takes as a method value is dead, yet still compiles because the service
// must satisfy the interface.
func TestConsoleAPIPortMethodsAreAllUsedByHTTPAPI(t *testing.T) {
	root := repoRoot(t)
	for _, port := range []string{"Admin", "Console", "PluginsService"} {
		t.Run(port, func(t *testing.T) {
			unused, problems := unusedMethods(t, root, port)
			for _, problem := range problems {
				t.Errorf("consoleapi.%s, so this test cannot list its methods", problem)
			}
			for _, method := range unused {
				t.Errorf("consoleapi.%s is neither called nor referenced in internal/httpapi; remove it from its interface, and from its implementation too if nothing else calls it", method)
			}
		})
	}
}

// unusedMethods lists, as Interface.Method, each method of the port
// interface in root's internal/consoleapi that no selector in a non-test
// file of root's internal/httpapi names. The port's methods include those
// of every interface it embeds, however deep, from the non-test files of
// its own package; any other embedded type is a problem, since its methods
// cannot be listed without type information.
//
// The check matches selector names without type information: a method or
// field of the same name on any other type in httpapi also counts as a use,
// so it can miss a dead method with a common name, never flag a live one.
func unusedMethods(t *testing.T, root, port string) (unused, problems []string) {
	t.Helper()
	interfaces := map[string]*ast.InterfaceType{}
	for _, src := range parseGoSources(t, root, packageSourceFiles(t, filepath.Join(root, "internal", "consoleapi"))) {
		for _, decl := range src.syntax.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				if iface, ok := typeSpec.Type.(*ast.InterfaceType); ok {
					interfaces[typeSpec.Name.Name] = iface
				}
			}
		}
	}
	if interfaces[port] == nil {
		t.Fatalf("internal/consoleapi declares no %s interface", port)
	}
	declared := map[string]string{}
	visited := map[string]bool{}
	var collect func(name string)
	collect = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		for _, field := range interfaces[name].Methods.List {
			for _, method := range field.Names {
				declared[method.Name] = name
			}
			if len(field.Names) > 0 {
				continue
			}
			if embedded, ok := field.Type.(*ast.Ident); ok && interfaces[embedded.Name] != nil {
				collect(embedded.Name)
				continue
			}
			problems = append(problems, fmt.Sprintf("%s embeds %s, which is not an interface declared in internal/consoleapi", name, types.ExprString(field.Type)))
		}
	}
	collect(port)
	if len(declared) == 0 {
		t.Fatalf("consoleapi.%s declares no methods", port)
	}
	used := map[string]bool{}
	for _, src := range parseGoSources(t, root, packageSourceFiles(t, filepath.Join(root, "internal", "httpapi"))) {
		ast.Inspect(src.syntax, func(n ast.Node) bool {
			if selector, ok := n.(*ast.SelectorExpr); ok {
				used[selector.Sel.Name] = true
			}
			return true
		})
	}
	for method, iface := range declared {
		if !used[method] {
			unused = append(unused, iface+"."+method)
		}
	}
	sort.Strings(unused)
	return unused, problems
}

// packageSourceFiles lists the non-test Go files of the package in dir.
func packageSourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		if name := entry.Name(); !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	return files
}

// The methods of consoleapi.Admin include those of every interface it
// embeds from its own package, however deep; an embedded interface from
// elsewhere cannot be listed without type information and is reported.
func TestAdminMethodsIncludeThoseOfEmbeddedInterfaces(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal", "architecture", "testdata", "admin_methods")
	unused, problems := unusedMethods(t, root, "Admin")
	if want := []string{"Deeper.Unused"}; fmt.Sprint(unused) != fmt.Sprint(want) {
		t.Errorf("unused = %v, want %v", unused, want)
	}
	if want := []string{"Admin embeds io.Closer, which is not an interface declared in internal/consoleapi"}; fmt.Sprint(problems) != fmt.Sprint(want) {
		t.Errorf("problems = %v, want %v", problems, want)
	}
	// Another port reaches Deeper through Extra, without Admin's io.Closer.
	unused, problems = unusedMethods(t, root, "Extra")
	if fmt.Sprint(unused) != "[Deeper.Unused]" || len(problems) != 0 {
		t.Errorf("Extra: unused = %v, problems = %v", unused, problems)
	}
}

func TestInlineStateLiteralsOnlyShrink(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	for _, file := range sourceFiles(t, root) {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, file)
		ast.Inspect(parsed, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{bin.X, bin.Y} {
				lit, ok := side.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				word, err := strconv.Unquote(lit.Value)
				if err == nil && stateWords[word] {
					offenders = append(offenders, fmt.Sprintf("%s:%d %s", filepath.ToSlash(rel), fset.Position(bin.Pos()).Line, word))
				}
			}
			return true
		})
	}
	ratchet(t, "state_literals", offenders)
}

func TestCompositionRootOnlyShrinks(t *testing.T) {
	root := repoRoot(t)
	lines := 0
	imports := map[string]bool{}
	for _, file := range sourceFiles(t, root) {
		rel, _ := filepath.Rel(root, file)
		if !strings.HasPrefix(filepath.ToSlash(rel), "cmd/steve/") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines += strings.Count(string(raw), "\n")
		parsed, err := parser.ParseFile(token.NewFileSet(), file, raw, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if strings.HasPrefix(path, "github.com/gopact-ai/steve/internal/") {
				imports[path] = true
			}
		}
	}
	if lines > cmdSteveMaxLines {
		t.Errorf("cmd/steve has %d non-test lines; the composition root may not grow past %d", lines, cmdSteveMaxLines)
	}
	if len(imports) > cmdSteveMaxFanOut {
		t.Errorf("cmd/steve imports %d internal packages; it may not wire more than %d directly", len(imports), cmdSteveMaxFanOut)
	}
}
