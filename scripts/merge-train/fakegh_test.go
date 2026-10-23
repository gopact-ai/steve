package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake gh: with FAKE_GH_WORLD set it handles
// one gh invocation against a JSON world file and exits. No network.
func TestMain(m *testing.M) {
	if world := os.Getenv("FAKE_GH_WORLD"); world != "" {
		os.Exit(fakeGH(world, os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakePR struct {
	PR       PR       `json:"pr"`
	Files    []string `json:"files"`
	Threads  int      `json:"threads"`
	Checks   []Check  `json:"checks"`   // final results of the current head
	Required []string `json:"required"` // names of required checks
	Pending  int      `json:"pending"`  // check reads before results appear
	// What an update produces: conflicts make the branch DIRTY instead.
	UpdateConflicts bool    `json:"update_conflicts"`
	UpdateRefused   bool    `json:"update_refused"`
	UpdateChecks    []Check `json:"update_checks"`
	Blocked         bool    `json:"blocked"` // protection blocks with green checks
	Updates         int     `json:"updates"`
}

type world struct {
	PRs   map[string]*fakePR `json:"prs"`
	Calls [][]string         `json:"calls"`
	Base  int                `json:"base"` // bumps on every merge

	stdout, stderr io.Writer
}

func fakeGH(path string, args []string) int {
	code, _ := fakeGHTo(path, args, os.Stdout, os.Stderr)
	return code
}

// fakeGHTo runs one call against the world file, writing like gh would.
func fakeGHTo(path string, args []string, stdout, stderr io.Writer) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 3, err
	}
	var w world
	if err := json.Unmarshal(b, &w); err != nil {
		return 3, err
	}
	w.Calls = append(w.Calls, args)
	w.stdout, w.stderr = stdout, stderr
	code := w.handle(args)
	b, _ = json.Marshal(&w)
	return code, os.WriteFile(path, b, 0o644)
}

func (w *world) pr(s string) *fakePR { return w.PRs[s] }

func has(args []string, flag string) bool { return slices.Contains(args, flag) }

func (w *world) handle(args []string) int {
	out := func(v any) int { b, _ := json.Marshal(v); w.stdout.Write(b); return 0 }
	if len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
		return out(map[string]any{"owner": map[string]string{"login": "o"}, "name": "r"})
	}
	if len(args) >= 2 && args[0] == "api" && args[1] == "graphql" {
		n := ""
		for _, a := range args {
			if strings.HasPrefix(a, "n=") {
				n = strings.TrimPrefix(a, "n=")
			}
		}
		p := w.pr(n)
		if p == nil {
			return 1
		}
		nodes := []map[string]bool{{"isResolved": true}}
		for i := 0; i < p.Threads; i++ {
			nodes = append(nodes, map[string]bool{"isResolved": false})
		}
		return out(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": len(nodes), "pageInfo": map[string]bool{"hasNextPage": false}, "nodes": nodes}}}}})
	}
	if len(args) < 3 || args[0] != "pr" {
		fmt.Fprintln(w.stderr, "fake gh: unsupported", args)
		return 2
	}
	p := w.pr(args[2])
	if p == nil {
		fmt.Fprintln(w.stderr, "fake gh: no pull request", args[2])
		return 1
	}
	switch args[1] {
	case "view":
		return out(p.PR)
	case "diff":
		fmt.Fprintln(w.stdout, strings.Join(p.Files, "\n"))
		return 0
	case "checks":
		var rows []Check
		for _, c := range p.Checks {
			if has(args, "--required") && !slices.Contains(p.Required, c.Name) {
				continue
			}
			if p.Pending > 0 {
				c.Bucket, c.State = "pending", "IN_PROGRESS"
			}
			rows = append(rows, c)
		}
		if p.Pending > 0 {
			p.Pending--
		}
		out(rows)
		for _, c := range rows {
			if c.Bucket != "pass" {
				return 8 // like gh: non-zero for failing or pending checks
			}
		}
		return 0
	case "update-branch":
		if has(args, "--rebase") || p.PR.MergeStateStatus != "BEHIND" {
			return 1
		}
		if p.UpdateRefused { // GitHub's answer to a conflicting update
			p.PR.MergeStateStatus = "DIRTY"
			fmt.Fprintln(w.stderr, "merge conflict between base and head")
			return 1
		}
		p.Updates++
		p.PR.HeadRefOid = fmt.Sprintf("%s-update%d", p.PR.HeadRefOid, p.Updates)
		if p.UpdateConflicts {
			p.PR.MergeStateStatus = "DIRTY"
			return 0
		}
		p.PR.MergeStateStatus = "CLEAN"
		if p.Blocked {
			p.PR.MergeStateStatus = "BLOCKED"
		}
		if p.UpdateChecks != nil {
			p.Checks = p.UpdateChecks
		}
		p.Pending = 2
		return 0
	case "merge":
		// Branch protection, as GitHub enforces it without --admin.
		if has(args, "--admin") || has(args, "--auto") || !has(args, "--merge") {
			fmt.Fprintln(w.stderr, "fake gh: forbidden merge flags", args)
			return 1
		}
		i := slices.Index(args, "--match-head-commit")
		if i < 0 || i+1 >= len(args) || args[i+1] != p.PR.HeadRefOid {
			fmt.Fprintln(w.stderr, "fake gh: head moved")
			return 1
		}
		if p.PR.MergeStateStatus != "CLEAN" || p.Pending > 0 {
			fmt.Fprintln(w.stderr, "fake gh: protection: not mergeable", p.PR.MergeStateStatus)
			return 1
		}
		p.PR.State = "MERGED"
		w.Base++
		for _, o := range w.PRs {
			if o.PR.State == "OPEN" && o.PR.MergeStateStatus == "CLEAN" {
				o.PR.MergeStateStatus = "BEHIND"
			}
		}
		return 0
	}
	fmt.Fprintln(w.stderr, "fake gh: unsupported", args)
	return 2
}

// harness wires a Train to the fake gh binary.
type harness struct {
	t     *testing.T
	world string
	state string
	out   strings.Builder
}

func green(names ...string) []Check {
	var cs []Check
	for _, n := range names {
		cs = append(cs, Check{Name: n, Bucket: "pass", State: "SUCCESS", Link: "https://ci.example/" + n})
	}
	return cs
}

var requiredChecks = []string{"test", "console", "desktop", "macos", "fleet"}

// open is a ready-to-merge pull request on master unless changed.
func open(n int, status string, files ...string) *fakePR {
	return &fakePR{
		PR:       PR{Number: n, Title: fmt.Sprintf("PR %d", n), URL: fmt.Sprintf("https://github.example/pull/%d", n), State: "OPEN", MergeStateStatus: status, HeadRefName: fmt.Sprintf("branch-%d", n), HeadRefOid: fmt.Sprintf("sha%d", n), BaseRefName: "master"},
		Files:    files,
		Checks:   green(requiredChecks...),
		Required: requiredChecks,
	}
}

func newHarness(t *testing.T, prs ...*fakePR) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, world: filepath.Join(dir, "world.json"), state: filepath.Join(dir, "merge-train.json")}
	w := world{PRs: map[string]*fakePR{}}
	for _, p := range prs {
		w.PRs[strconv.Itoa(p.PR.Number)] = p
	}
	h.write(&w)
	return h
}

func (h *harness) read() *world {
	h.t.Helper()
	b, err := os.ReadFile(h.world)
	if err != nil {
		h.t.Fatal(err)
	}
	var w world
	if err := json.Unmarshal(b, &w); err != nil {
		h.t.Fatal(err)
	}
	return &w
}

func (h *harness) write(w *world) {
	h.t.Helper()
	b, _ := json.Marshal(w)
	if err := os.WriteFile(h.world, b, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// train runs the real cli argument code against the fake in-process;
// TestCLIAgainstFakeGHBinary covers the same fake as a subprocess.
func (h *harness) train(apply bool, list ...int) *Train {
	run := func(args []string) ([]byte, []byte, error) {
		var out, errOut bytes.Buffer
		code, err := fakeGHTo(h.world, args, &out, &errOut)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit status %d", code)
		}
		return out.Bytes(), errOut.Bytes(), err
	}
	return &Train{GH: &cli{bin: "gh", exec: run}, List: list, Apply: apply, State: h.state, Out: &h.out, Poll: time.Millisecond, Timeout: time.Hour, Sleep: func(time.Duration) {}}
}

// subprocess makes the same train call the test binary as its gh.
func (h *harness) subprocess(tr *Train) *Train {
	h.t.Setenv("FAKE_GH_WORLD", h.world)
	bin, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}
	tr.GH = &cli{bin: bin}
	return tr
}
