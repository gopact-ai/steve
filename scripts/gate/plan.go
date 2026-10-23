package main

import (
	"fmt"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
)

type plan struct {
	full       bool
	fullWhy    []string
	goChanged  map[string]bool // module packages whose files changed
	goTest     []string        // packages to vet and test
	vetGate    bool
	console    []*consoleStep
	consoleAll bool
	consoleWhy []string
	desktop    bool
	providers  []string
	skipped    []string
	notes      []string
}

func buildPlan(files []string) (*plan, error) {
	p := &plan{goChanged: map[string]bool{}}
	for _, f := range files {
		if fullSuite(f) {
			p.full = true
			p.fullWhy = append(p.fullWhy, f)
		}
	}
	if p.full {
		return p, nil
	}
	g, err := loadGoGraph()
	if err != nil {
		return nil, err
	}
	suite, err := loadConsole()
	if err != nil {
		return nil, err
	}
	direct := map[string]bool{} // packages tested without reverse dependencies
	consoleHit := map[*consoleStep]bool{}
	goSource := false
	for _, f := range files {
		placed := false
		if strings.HasPrefix(f, "scripts/gate/") {
			p.vetGate = path.Ext(f) == ".go"
			continue
		}
		if inert(f) && !g.embeds[f] && !dataRead(f) {
			p.skipped = append(p.skipped, f)
			continue
		}
		if strings.HasPrefix(f, consoleDist) {
			for _, pkg := range embedReaders {
				direct[module+"/"+pkg] = true
			}
			placed = true
		} else if pkg := g.packageOf(f); pkg != "" {
			p.goChanged[pkg] = true
			goSource = goSource || path.Ext(f) == ".go"
			placed = true
		}
		for _, r := range dataReaders {
			if strings.HasPrefix(f, r.prefix) {
				for _, pkg := range r.pkgs {
					direct[module+"/"+pkg] = true
				}
				placed = true
			}
		}
		if consoleFull(f) {
			p.consoleAll = true
			p.consoleWhy = append(p.consoleWhy, f)
			placed = true
		}
		for _, step := range suite.steps {
			if step.reach.has(f) || step.script == "build" && buildInput(f) {
				consoleHit[step] = true
				placed = true
			}
		}
		if !placed && consoleInput(f) {
			p.consoleAll = true
			p.consoleWhy = append(p.consoleWhy, f+" (no console script reaches it)")
			placed = true
		}
		if desktopInput(f) {
			p.desktop = true
			placed = true
		}
		if w := separateWorkflow(f); w != "" {
			p.providers = appendOnce(p.providers, strings.TrimPrefix(w, "npm test in "))
			placed = true
		}
		if !placed {
			p.full = true
			p.fullWhy = append(p.fullWhy, f+" (not mapped)")
		}
	}
	if p.full {
		return p, nil
	}
	if goSource {
		for _, pkg := range sourceReaders {
			direct[module+"/"+pkg] = true
		}
	}
	selected := map[string]bool{}
	for _, pkg := range g.affected(p.goChanged) {
		selected[pkg] = true
	}
	for pkg := range direct {
		if _, ok := g.reach[pkg]; ok {
			selected[pkg] = true
		}
	}
	for pkg := range selected {
		p.goTest = append(p.goTest, pkg)
	}
	sort.Strings(p.goTest)
	// Console steps that build or test Go code see Go changes too.
	for _, step := range suite.steps {
		for _, built := range step.goPkgs {
			for c := range p.goChanged {
				if g.reach[built][c] {
					consoleHit[step] = true
				}
			}
		}
	}
	for _, step := range suite.steps {
		if p.consoleAll || consoleHit[step] {
			p.console = append(p.console, step)
		}
	}
	if p.desktop && runtime.GOOS != "darwin" {
		p.notes = append(p.notes, "desktop files changed; make test-desktop needs macOS, so CI's desktop job is the check")
		p.desktop = false
	}
	return p, nil
}

func appendOnce(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func (p *plan) print(base string, n int) {
	fmt.Printf("gate: %d changed file(s) against %s\n", n, base)
	if p.full {
		fmt.Println("gate: full suite (make gate-full) because of:")
		for _, f := range p.fullWhy {
			fmt.Println("  ", f)
		}
		return
	}
	var changed []string
	for c := range p.goChanged {
		changed = append(changed, short(c))
	}
	sort.Strings(changed)
	fmt.Printf("go: changed %v\n", changed)
	fmt.Printf("go: vet + test -race %d package(s)", len(p.goTest))
	if len(p.goTest) > 0 && len(p.goTest) <= 40 {
		var s []string
		for _, t := range p.goTest {
			s = append(s, short(t))
		}
		fmt.Printf(": %s", strings.Join(s, " "))
	}
	fmt.Println()
	if p.consoleAll {
		fmt.Printf("console: all scripts because of %v\n", p.consoleWhy)
	}
	if len(p.console) > 0 {
		fmt.Printf("console: %d step(s) from %s\n", len(p.console), strings.Join(p.consoleScripts(), " "))
	}
	if p.desktop {
		fmt.Println("desktop: make test-desktop + package app icon")
	}
	for _, d := range p.providers {
		fmt.Printf("provider: npm test in %s\n", d)
	}
	if len(p.skipped) > 0 {
		fmt.Printf("no automated check reads: %s\n", strings.Join(p.skipped, " "))
	}
	for _, n := range p.notes {
		fmt.Println("note:", n)
	}
}

func (p *plan) consoleScripts() []string {
	var out []string
	for _, s := range p.console {
		out = appendOnce(out, s.script)
	}
	return out
}

func fileExists(f string) bool { _, err := os.Stat(f); return err == nil }
