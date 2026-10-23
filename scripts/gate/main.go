// Command gate runs the checks that a change can affect, as a fast local
// loop before CI. It reads the files changed since the merge base with
// origin/master (plus uncommitted and untracked files), selects the Go
// packages whose tests can observe them and the console scripts whose
// import graph reaches them, and runs gofmt, go vet, go test -race and those
// scripts. Anything it cannot place falls back to scripts/gate/full.sh,
// which mirrors the CI jobs. CI stays the final authority.
//
//	go run ./scripts/gate            # what make gate runs
//	go run ./scripts/gate -n         # print the plan only
//	go run ./scripts/gate -base HEAD # only uncommitted changes
//	go run ./scripts/gate -n -files internal/tuiapp/app.go
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	base := flag.String("base", envOr("GATE_BASE", "origin/master"), "compare against the merge base with this ref")
	dry := flag.Bool("n", false, "print the plan without running it")
	only := flag.String("files", "", "comma-separated paths to plan for instead of the git diff")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fail(err)
	}
	if err := os.Chdir(root); err != nil {
		fail(err)
	}
	started := time.Now()
	files, err := changedFiles(*base)
	if *only != "" {
		files, err = strings.Split(*only, ","), nil
	}
	if err != nil {
		fail(err)
	}
	p, err := buildPlan(files)
	if err != nil {
		fail(err)
	}
	p.print(*base, len(files))
	if *dry {
		return
	}
	if p.full {
		cmd := exec.Command("bash", "scripts/gate/full.sh")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		fmt.Printf("\ngate: full suite %s in %s\n", verdict(err == nil), since(started))
		if err != nil {
			os.Exit(1)
		}
		return
	}
	ok := p.run()
	fmt.Printf("\ngate: %s in %s\n", verdict(ok), since(started))
	if !ok {
		os.Exit(1)
	}
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git checkout: %w", err)
	}
	return filepath.Clean(strings.TrimSpace(string(out))), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func verdict(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func since(t time.Time) string { return time.Since(t).Round(100 * time.Millisecond).String() }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gate:", err)
	os.Exit(2)
}
