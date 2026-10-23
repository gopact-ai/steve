package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const webDir = "web/console"

// consoleStep is one command of make test-console: a whole npm script
// (build) or one && segment of a test:* script after expanding npm run.
type consoleStep struct {
	script  string // the make test-console entry it belongs to
	command string // run with sh -c in web/console
	files   []string
	reach   reach
	goPkgs  []string // module packages it builds or tests with the go tool
}

// reach is what a step can observe: files and whole directories.
type reach struct {
	files map[string]bool
	dirs  []string
}

func (r reach) has(f string) bool {
	if r.files[f] {
		return true
	}
	for _, d := range r.dirs {
		if strings.HasPrefix(f, d+"/") {
			return true
		}
	}
	return false
}

type consoleSuite struct {
	order []string // make test-console scripts, in order
	steps []*consoleStep
	graph *jsGraph
}

// loadConsole reads the make test-console order from the Makefile and each
// script from web/console/package.json.
func loadConsole() (*consoleSuite, error) {
	raw, err := os.ReadFile(filepath.Join(webDir, "package.json"))
	if err != nil {
		return nil, err
	}
	var pkg struct{ Scripts map[string]string }
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, err
	}
	order, err := makeTestConsole()
	if err != nil {
		return nil, err
	}
	s := &consoleSuite{order: order, graph: newJSGraph()}
	for _, script := range order {
		if script == "build" {
			s.steps = append(s.steps, &consoleStep{script: script, command: "npm run build"})
			continue
		}
		body, ok := pkg.Scripts[script]
		if !ok {
			return nil, fmt.Errorf("make test-console runs %q, which web/console/package.json does not define", script)
		}
		for _, cmd := range expand(body, pkg.Scripts, 0) {
			step := &consoleStep{script: script, command: cmd, files: nodeFiles(cmd)}
			s.steps = append(s.steps, step)
		}
	}
	for _, step := range s.steps {
		step.reach, step.goPkgs = s.graph.closure(step.files)
	}
	return s, nil
}

var npmRun = regexp.MustCompile(`^npm --prefix web/console run (\S+)$`)

func makeTestConsole() ([]string, error) {
	raw, err := os.ReadFile("Makefile")
	if err != nil {
		return nil, err
	}
	var order []string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "test-console:"):
			in = true
		case in && strings.HasPrefix(line, "\t"):
			if m := npmRun.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				order = append(order, m[1])
			}
		case in:
			in = false
		}
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("no npm scripts found under make test-console")
	}
	return order, nil
}

// expand splits a script on && and inlines npm run <script>.
func expand(body string, scripts map[string]string, depth int) []string {
	var out []string
	for _, part := range strings.Split(body, "&&") {
		part = strings.TrimSpace(part)
		if name, ok := strings.CutPrefix(part, "npm run "); ok && depth < 8 {
			if inner, ok := scripts[strings.TrimSpace(name)]; ok {
				out = append(out, expand(inner, scripts, depth+1)...)
				continue
			}
		}
		out = append(out, part)
	}
	return out
}

// nodeFiles are the repo-relative scripts a "node [--test] a b" command runs.
func nodeFiles(cmd string) []string {
	var files []string
	for _, f := range strings.Fields(cmd) {
		if strings.HasSuffix(f, ".mjs") || strings.HasSuffix(f, ".js") {
			files = append(files, path.Clean(path.Join(webDir, f)))
		}
	}
	return files
}

// jsGraph resolves the static and dynamic imports of console sources and
// test scripts. Test scripts also count every path literal they mention.
type jsGraph struct {
	edges map[string]*jsNode
}

type jsNode struct {
	files  []string
	dirs   []string
	goPkgs []string
}

func newJSGraph() *jsGraph { return &jsGraph{edges: map[string]*jsNode{}} }

var (
	importSpec = regexp.MustCompile(`(?:\bfrom\s*|\bimport\s*\(?\s*|@import\s+|@source\s+)["'\x60]([^"'\x60$]+)["'\x60]`)
	pathLit    = regexp.MustCompile(`["'\x60]([^"'\x60\s$]*[A-Za-z0-9_/.-]+)["'\x60]`)
	srcMention = regexp.MustCompile(`(?:@|/src)/[A-Za-z0-9_./-]+`)
	goCommand  = regexp.MustCompile(`["']\./((?:internal|e2e|cmd)/[A-Za-z0-9_/-]+)["']`)
)

// A browser test that serves web/console through its own vite config, or
// serves the built dist, can load any module of the application.
func servesApp(src string) bool {
	vite := strings.Contains(src, "createServer(") && strings.Contains(src, "vite")
	scratch := regexp.MustCompile(`root:\s*scratch`).MatchString(src)
	return vite && !scratch
}

func (g *jsGraph) node(file string) *jsNode {
	if n, ok := g.edges[file]; ok {
		return n
	}
	n := &jsNode{}
	g.edges[file] = n
	raw, err := os.ReadFile(file)
	if err != nil {
		return n
	}
	src := string(raw)
	isTest := !strings.HasPrefix(file, webDir+"/src/")
	add := func(f string) {
		if f == "" {
			return
		}
		info, err := os.Stat(f)
		if err != nil {
			return
		}
		if info.IsDir() {
			n.dirs = append(n.dirs, f)
		} else {
			n.files = append(n.files, f)
		}
	}
	for _, m := range importSpec.FindAllStringSubmatch(src, -1) {
		add(resolve(file, m[1]))
	}
	if isTest {
		for _, m := range pathLit.FindAllStringSubmatch(src, -1) {
			if strings.Contains(m[1], "node_modules") || !strings.Contains(m[1], "/") && !strings.HasPrefix(m[1], ".") {
				continue
			}
			add(resolve(file, m[1]))
		}
		for _, m := range srcMention.FindAllString(src, -1) {
			add(resolve(file, m))
		}
		for _, m := range goCommand.FindAllStringSubmatch(src, -1) {
			n.goPkgs = append(n.goPkgs, module+"/"+m[1])
		}
		if servesApp(src) {
			add(webDir + "/index.html")
			add(webDir + "/src/main.tsx")
		}
		if strings.Contains(src, "readdirSync") || strings.Contains(src, "readdir(") {
			// Directory scans: count the console source as a whole.
			n.dirs = append(n.dirs, webDir+"/src")
		}
	}
	if file == webDir+"/index.html" {
		add(webDir + "/src/main.tsx")
	}
	return n
}

// resolve maps an import or path literal to a repo-relative file or dir.
func resolve(from, spec string) string {
	var bases []string
	switch {
	case strings.HasPrefix(spec, "@/"):
		bases = []string{path.Join(webDir, "src", spec[2:])}
	case strings.HasPrefix(spec, "/src/"):
		bases = []string{path.Join(webDir, spec)}
	case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		bases = []string{path.Join(path.Dir(from), spec)}
	case strings.HasPrefix(from, webDir+"/src/"):
		return "" // a package import
	default:
		// Test scripts also join plain literals onto web/console or the root.
		// A bare top-level name ("providers/") is a pattern, not a path.
		bases = []string{path.Join(webDir, spec)}
		if strings.Contains(strings.Trim(spec, "/"), "/") {
			bases = append(bases, path.Clean(spec))
		}
	}
	for _, base := range bases {
		if strings.HasPrefix(base, "..") || base == "." || base == webDir || base == "web" {
			continue
		}
		for _, ext := range []string{"", ".ts", ".tsx", ".mjs", ".js", ".css", "/index.ts", "/index.tsx"} {
			if info, err := os.Stat(base + ext); err == nil && (ext == "" || !info.IsDir()) {
				return base + ext
			}
		}
	}
	return ""
}

// closure is everything reachable from files, plus the Go packages built or
// tested along the way. The built dist depends on the whole console source.
func (g *jsGraph) closure(files []string) (reach, []string) {
	r := reach{files: map[string]bool{}}
	goPkgs := map[string]bool{}
	stack := append([]string{}, files...)
	dirs := map[string]bool{}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if r.files[f] {
			continue
		}
		r.files[f] = true
		n := g.node(f)
		stack = append(stack, n.files...)
		for _, d := range n.dirs {
			dirs[d] = true
		}
		for _, p := range n.goPkgs {
			goPkgs[p] = true
		}
	}
	if dirs[strings.TrimSuffix(consoleDist, "/")] {
		dirs[webDir+"/src"] = true
		r.files[webDir+"/index.html"] = true
	}
	for d := range dirs {
		r.dirs = append(r.dirs, d)
	}
	sort.Strings(r.dirs)
	var pkgs []string
	for p := range goPkgs {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	return r, pkgs
}

// consoleInput is a path that console checks read, so a change that no step
// reaches is a gap in the mapping rather than an unread file.
func consoleInput(f string) bool {
	for _, p := range []string{webDir + "/", "e2e/console/", "e2e/childcard/"} {
		if strings.HasPrefix(f, p) {
			return true
		}
	}
	return false
}

// buildInput changes the output of npm run build.
func buildInput(f string) bool {
	return strings.HasPrefix(f, webDir+"/src/") || f == webDir+"/index.html" || f == webDir+"/scripts/generate-palettes.mjs" || consoleFull(f)
}
