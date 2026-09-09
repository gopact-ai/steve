package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The ratchets below hold the line on what the refactoring plan
// (docs/refactor.md) is removing: they fail when a new offender appears
// and when the baseline still lists one that is gone. Regenerate a
// baseline after removing offenders with RATCHET_UPDATE=1.

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
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
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
	// An entry is "name" or "name size"; a sized entry may shrink below its
	// baseline without touching the file, but not grow past it.
	split := func(entry string) (string, int) {
		if i := strings.LastIndexByte(entry, ' '); i >= 0 {
			if size, err := strconv.Atoi(entry[i+1:]); err == nil {
				return entry[:i], size
			}
		}
		return entry, 0
	}
	baseline := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			key, size := split(line)
			baseline[key] = size
		}
	}
	seen := map[string]bool{}
	for _, item := range current {
		key, size := split(item)
		seen[key] = true
		limit, listed := baseline[key]
		switch {
		case !listed:
			t.Errorf("%s: new offender %s — fix it rather than adding it to the baseline", name, item)
		case size > limit:
			t.Errorf("%s: %s grew past its baseline (%d > %d)", name, key, size, limit)
		}
	}
	for key := range baseline {
		if !seen[key] {
			t.Errorf("%s: %s is no longer an offender; remove it from testdata/%s.txt", name, key, name)
		}
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
