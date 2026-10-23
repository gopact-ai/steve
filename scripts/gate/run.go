package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// run executes the plan in the order CI would, stopping at nothing: every
// selected check runs so one pass reports every failure.
func (p *plan) run() bool {
	ok := true
	section := func(name string, f func() bool) {
		fmt.Printf("\n== %s\n", name)
		start := time.Now()
		pass := f()
		fmt.Printf("-- %s %s (%s)\n", name, verdict(pass), since(start))
		ok = ok && pass
	}
	if len(p.goTest) > 0 || p.vetGate {
		section("gofmt", gofmtCheck)
	}
	if p.vetGate {
		section("go vet scripts/gate", func() bool { return sh(".", "go", "vet", "./scripts/gate") })
	}
	if len(p.goTest) > 0 {
		section("go vet", func() bool { return sh(".", "go", append([]string{"vet"}, p.goTest...)...) })
		section("go test -race", func() bool { return goTest(p.goTest) })
	}
	if len(p.console) > 0 {
		section("console", p.runConsole)
	}
	if p.desktop {
		section("desktop", func() bool { return sh(".", "make", "test-desktop") && packageIcon() })
	}
	for _, dir := range p.providers {
		section("provider "+dir, func() bool {
			return sh(dir, "npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund") && sh(dir, "npm", "test")
		})
	}
	return ok
}

// gofmtCheck is CI's gofmt step, over tracked and new Go files.
func gofmtCheck() bool {
	out, err := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "*.go").Output()
	if err != nil {
		fmt.Println(err)
		return false
	}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		if fileExists(f) {
			files = append(files, f)
		}
	}
	cmd := exec.Command("gofmt", append([]string{"-l"}, files...)...)
	cmd.Stderr = os.Stderr
	bad, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(bad)) > 0 {
		fmt.Printf("not gofmt-formatted:\n%s", bad)
		return false
	}
	return true
}

func sh(dir, name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run() == nil
}

type testEvent struct {
	Action, Package, Test, Output, FailedBuild string
	ImportPath                                 string // build-output events
	Elapsed                                    float64
}

type pkgResult struct {
	action  string
	elapsed float64
	failed  []string            // failing tests, in order
	output  map[string][]string // test ("" for the package) -> lines
}

// goTest runs make test's command on the selected packages, keeping the
// JSON stream on disk and printing one line per package.
func goTest(pkgs []string) bool {
	dir := filepath.Join(os.TempDir(), "steve-gate")
	_ = os.MkdirAll(dir, 0o755)
	jsonPath := filepath.Join(dir, "go-test.json")
	out, err := os.Create(jsonPath)
	if err != nil {
		fmt.Println(err)
		return false
	}
	defer out.Close()
	cmd := exec.Command("go", append([]string{"test", "-race", "-timeout", "30m", "-json"}, pkgs...)...)
	cmd.Stderr = os.Stderr
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		fmt.Println(err)
		return false
	}
	results := map[string]*pkgResult{}
	get := func(p string) *pkgResult {
		r := results[p]
		if r == nil {
			r = &pkgResult{output: map[string][]string{}}
			results[p] = r
		}
		return r
	}
	scan := bufio.NewScanner(io.TeeReader(stdout, out))
	scan.Buffer(make([]byte, 1<<20), 16<<20)
	for scan.Scan() {
		var e testEvent
		if json.Unmarshal(scan.Bytes(), &e) != nil {
			continue
		}
		if e.Package == "" && e.ImportPath != "" {
			e.Package = strings.TrimSuffix(stripVariant(e.ImportPath), ".test")
		}
		if e.Package == "" {
			continue
		}
		r := get(e.Package)
		switch e.Action {
		case "output":
			if len(r.output[e.Test]) < 400 {
				r.output[e.Test] = append(r.output[e.Test], e.Output)
			}
		case "build-output":
			r.output[""] = append(r.output[""], e.Output)
		case "pass", "fail", "skip":
			if e.Test == "" {
				r.action, r.elapsed = e.Action, e.Elapsed
				fmt.Printf("%-4s %7.1fs  %s\n", strings.ToUpper(e.Action), e.Elapsed, short(e.Package))
			} else if e.Action == "fail" {
				r.failed = append(r.failed, e.Test)
			}
		}
	}
	waitErr := cmd.Wait()
	var names []string
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)
	failed := false
	for _, name := range names {
		r := results[name]
		if r.action != "fail" && r.action != "" {
			continue
		}
		if r.action == "" && len(r.output[""]) == 0 {
			continue
		}
		failed = true
		fmt.Printf("\n--- FAIL %s", short(name))
		lines := r.output[""]
		if len(r.failed) > 0 {
			first := leaf(r.failed)
			fmt.Printf(": %s", first)
			lines = r.output[first]
			if len(r.failed) > 1 {
				fmt.Printf(" (+%d more)", len(r.failed)-1)
			}
		}
		fmt.Println()
		for _, l := range lines {
			fmt.Print("    ", l)
		}
	}
	fmt.Printf("json: %s\n", jsonPath)
	return waitErr == nil && !failed
}

// leaf prefers the first failing subtest over its failing parent.
func leaf(failed []string) string {
	for _, t := range failed {
		if strings.Contains(t, "/") {
			return t
		}
	}
	return failed[0]
}

// runConsole runs the selected steps in make test-console order, with
// make test-console's mail fixture install when test:mail is selected.
func (p *plan) runConsole() bool {
	if !fileExists(filepath.Join(webDir, "node_modules")) {
		fmt.Println("web/console/node_modules is missing; run: npm --prefix web/console ci && npx --prefix web/console playwright install chromium")
		return false
	}
	for _, s := range p.console {
		if s.script == "test:mail" {
			if !sh(".", "npm", "--prefix", "providers/mail", "ci", "--ignore-scripts", "--no-audit", "--no-fund") {
				return false
			}
			break
		}
	}
	ok := true
	for _, s := range p.console {
		start := time.Now()
		cmd := exec.Command("sh", "-c", s.command)
		cmd.Dir = webDir
		cmd.Env = os.Environ()
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err := cmd.Run()
		fmt.Printf("%-4s %7.1fs  %s: %s\n", verdict(err == nil), time.Since(start).Seconds(), s.script, s.command)
		if err != nil {
			ok = false
			lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
			if len(lines) > 60 {
				lines = append([]string{"..."}, lines[len(lines)-60:]...)
			}
			for _, l := range lines {
				fmt.Println("    " + l)
			}
		}
		if s.script == "build" && err == nil {
			if out, _ := exec.Command("git", "status", "--porcelain", "--", consoleDist).Output(); len(out) > 0 {
				fmt.Println("note: the build changed internal/readmodel/web/dist; commit it (CI fails on a stale dist)")
			}
		}
	}
	return ok
}

// packageIcon is the desktop job's icon packaging step, into a temporary
// directory the gate removes afterwards.
func packageIcon() bool {
	dir, err := os.MkdirTemp("", "steve-gate-icon-")
	if err != nil {
		fmt.Println(err)
		return false
	}
	defer os.RemoveAll(dir)
	return sh(".", "bash", "scripts/build-app-icon.sh", "desktop/macos/Assets/AppIcon.png", filepath.Join(dir, "AppIcon.icns"))
}
