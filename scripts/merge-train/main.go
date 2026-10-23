// Command merge-train merges an owner-approved list of pull requests with
// the fewest CI runs. master requires branches to be up to date, so each
// merge after the first needs one branch update and one CI run; the train
// does them one at a time, in an order that keeps overlapping changes in the
// owner's order and merges unrelated ones first.
//
// For each listed pull request, in order, it re-reads GitHub and:
//   - skips it if already merged (re-running is safe);
//   - stops on a draft, a closed pull request, a base other than -base,
//     requested changes, an unresolved review thread, or a red or cancelled
//     check (printing the failing job's URL);
//   - reports a conflict with the base and moves on: it never resolves one;
//   - if BEHIND, runs `gh pr update-branch` (a merge commit; never a rebase
//     or a force-push), waits for the new head's checks, then merges;
//   - merges with `gh pr merge --merge --match-head-commit <checked head>`,
//     never --admin or --auto, so branch protection decides as usual.
//
// It acts only on listed pull requests and only with -apply; without it it
// prints the plan. Progress is kept in .git/merge-train.json, so an
// interrupted run resumes with the same list.
//
//	go run ./scripts/merge-train 369 370 371          # plan only
//	go run ./scripts/merge-train -apply 369 370 371   # merge
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	apply := flag.Bool("apply", false, "update branches and merge; without it only print the plan")
	reset := flag.Bool("reset", false, "discard the saved train and plan afresh")
	repo := flag.String("repo", "", "OWNER/NAME (default: the current repository)")
	base := flag.String("base", "master", "branch the pull requests must target")
	poll := flag.Duration("poll", 30*time.Second, "how often to re-read checks while waiting")
	timeout := flag.Duration("timeout", 90*time.Minute, "longest wait for one pull request")
	state := flag.String("state", "", "state file (default: .git/merge-train.json)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: merge-train [-apply] [flags] PR...")
		flag.PrintDefaults()
	}
	flag.Parse()
	list, err := parseList(flag.Args())
	if err != nil {
		fail(err)
	}
	if *state == "" {
		if *state, err = statePath(); err != nil {
			fail(err)
		}
	}
	bin := os.Getenv("MERGE_TRAIN_GH")
	if bin == "" {
		bin = "gh"
	}
	t := &Train{GH: &cli{bin: bin, repo: *repo}, List: list, Base: *base, Apply: *apply, Reset: *reset, State: *state, Poll: *poll, Timeout: *timeout}
	if err := t.Run(); err != nil {
		var stop *Stop
		if errors.As(err, &stop) {
			fmt.Fprintln(os.Stderr, "merge-train: stopped at", stop.Error())
		} else {
			fmt.Fprintln(os.Stderr, "merge-train:", err)
		}
		os.Exit(1)
	}
}

// parseList accepts "369 370", "369,370" and "#369".
func parseList(args []string) ([]int, error) {
	var out []int
	for _, a := range args {
		for _, f := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.Atoi(strings.TrimPrefix(f, "#"))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%q is not a pull request number", f)
			}
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("list the owner-approved pull request numbers")
	}
	return out, nil
}

func gitOutput(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "merge-train:", err)
	os.Exit(2)
}
