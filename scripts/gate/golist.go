package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const module = "github.com/gopact-ai/steve"

// goPatterns are the packages make test and go vet cover in CI.
var goPatterns = []string{"./cmd/...", "./internal/...", "./e2e/..."}

type goPackage struct {
	ImportPath, ForTest, Dir, Name string
	Deps                           []string
	GoFiles, TestGoFiles           []string
	XTestGoFiles                   []string
	EmbedFiles, TestEmbedFiles     []string
	XTestEmbedFiles                []string
}

// goGraph knows, for each package make test runs, every module package its
// tests can observe: imports, test imports, and the closures of the
// binaries its tests build with go build (mockagent, the fixtures).
type goGraph struct {
	byDir   map[string]string          // repo-relative dir -> import path
	embeds  map[string]bool            // repo-relative embedded files
	reach   map[string]map[string]bool // package -> module packages its tests observe
	order   []string
	mainPkg map[string]bool
}

func loadGoGraph() (*goGraph, error) {
	args := append([]string{"list", "-e", "-test", "-json=ImportPath,ForTest,Dir,Name,Deps,GoFiles,TestGoFiles,XTestGoFiles,EmbedFiles,TestEmbedFiles,XTestEmbedFiles"}, goPatterns...)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	root, _ := os.Getwd()
	g := &goGraph{byDir: map[string]string{}, embeds: map[string]bool{}, reach: map[string]map[string]bool{}, mainPkg: map[string]bool{}}
	deps := map[string]map[string]bool{}
	files := map[string][]string{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p goPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		owner := owningPackage(p)
		if !strings.HasPrefix(owner, module+"/") {
			continue
		}
		set := deps[owner]
		if set == nil {
			set = map[string]bool{owner: true}
			deps[owner] = set
		}
		for _, d := range p.Deps {
			if d = stripVariant(d); strings.HasPrefix(d, module+"/") && !strings.HasSuffix(d, ".test") {
				set[d] = true
			}
		}
		if p.ImportPath == owner { // the plain package
			rel, _ := filepath.Rel(root, p.Dir)
			g.byDir[filepath.ToSlash(rel)] = owner
			g.order = append(g.order, owner)
			g.mainPkg[owner] = p.Name == "main"
			for _, f := range append(append(append([]string{}, p.EmbedFiles...), p.TestEmbedFiles...), p.XTestEmbedFiles...) {
				g.embeds[filepath.ToSlash(filepath.Join(rel, f))] = true
			}
			for _, f := range append(append(append([]string{}, p.GoFiles...), p.TestGoFiles...), p.XTestGoFiles...) {
				files[owner] = append(files[owner], filepath.Join(p.Dir, f))
			}
		}
	}
	for pkg, set := range deps {
		reach := map[string]bool{}
		for d := range set {
			reach[d] = true
		}
		for _, built := range builtBinaries(files[pkg], g) {
			for d := range deps[built] {
				reach[d] = true
			}
		}
		g.reach[pkg] = reach
	}
	sort.Strings(g.order)
	return g, nil
}

// owningPackage maps go list -test variants ("p [p.test]", "p.test",
// "p_test [p.test]") to the package whose tests they are.
func owningPackage(p goPackage) string {
	if p.ForTest != "" {
		return p.ForTest
	}
	ip := stripVariant(p.ImportPath)
	return strings.TrimSuffix(ip, ".test")
}

func stripVariant(ip string) string {
	if i := strings.Index(ip, " ["); i >= 0 {
		return ip[:i]
	}
	return ip
}

// binaryRef matches the package arguments tests pass to go build:
// "github.com/gopact-ai/steve/cmd/mockagent", "./cmd/mockagent",
// "../../cmd/mockagent", "./e2e/assistantfixture".
var binaryRef = regexp.MustCompile(`"(?:github\.com/gopact-ai/steve/|\./|(?:\.\./)+)((?:cmd|e2e|internal)/[A-Za-z0-9_/-]+)"`)

func builtBinaries(files []string, g *goGraph) []string {
	var out []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range binaryRef.FindAllStringSubmatch(string(raw), -1) {
			if ip := module + "/" + strings.TrimSuffix(m[1], "/"); g.mainPkg[ip] {
				out = append(out, ip)
			}
		}
	}
	return out
}

// packageOf is the package whose directory holds file (its source,
// testdata or embedded files), or "" when no tested package does.
func (g *goGraph) packageOf(file string) string {
	for dir := filepath.ToSlash(filepath.Dir(file)); dir != "." && dir != "/"; dir = filepath.ToSlash(filepath.Dir(dir)) {
		if ip, ok := g.byDir[dir]; ok {
			return ip
		}
	}
	return ""
}

// affected lists, in order, the packages whose tests observe any of changed.
func (g *goGraph) affected(changed map[string]bool) []string {
	var out []string
	for _, pkg := range g.order {
		for c := range changed {
			if g.reach[pkg][c] {
				out = append(out, pkg)
				break
			}
		}
	}
	return out
}

func short(ip string) string { return strings.TrimPrefix(ip, module+"/") }
