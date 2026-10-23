package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// State is the resumable record under .git/. GitHub stays the authority:
// every run re-reads each pull request, so a lost or stale file can only
// cost a re-read, never a second merge.
type State struct {
	List  []int `json:"list"`  // the owner-approved list, as given
	Order []int `json:"order"` // merge order fixed on the first run
	// Parents maps a stacked pull request to the listed one it is based on.
	Parents map[int]int       `json:"parents,omitempty"`
	Merged  map[int]string    `json:"merged,omitempty"`
	Notes   map[int]string    `json:"notes,omitempty"` // last outcome per pull request
	Update  map[int]time.Time `json:"updated,omitempty"`
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w (remove it or pass -reset)", path, err)
	}
	return &s, nil
}

func (s *State) save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Train merges the listed pull requests one at a time.
type Train struct {
	GH       GH
	List     []int
	Base     string // branch every pull request must target when merged
	Apply    bool
	Reset    bool
	State    string // path of the state file
	Poll     time.Duration
	Timeout  time.Duration // per wait for checks or for GitHub to settle
	Out      io.Writer
	Sleep    func(time.Duration)
	Now      func() time.Time
	listed   map[int]bool
	state    *State
	deadline time.Time
}

// Stop is the reason a run ended before merging everything.
type Stop struct {
	PR     int
	Reason string
	URL    string
}

func (s *Stop) Error() string {
	msg := fmt.Sprintf("#%d: %s", s.PR, s.Reason)
	if s.URL != "" {
		msg += "\n  " + s.URL
	}
	return msg
}

func (t *Train) logf(format string, args ...any) { fmt.Fprintf(t.Out, format+"\n", args...) }

func (t *Train) defaults() {
	if t.Out == nil {
		t.Out = os.Stdout
	}
	if t.Sleep == nil {
		t.Sleep = time.Sleep
	}
	if t.Now == nil {
		t.Now = time.Now
	}
	if t.Poll == 0 {
		t.Poll = 30 * time.Second
	}
	if t.Timeout == 0 {
		t.Timeout = 90 * time.Minute
	}
	if t.Base == "" {
		t.Base = "master"
	}
}

// Run plans and, with Apply, merges. It returns nil only when every listed
// pull request is merged (or, in a dry run, when the plan is complete).
func (t *Train) Run() error {
	t.defaults()
	if len(t.List) == 0 {
		return errors.New("no pull requests listed")
	}
	t.listed = map[int]bool{}
	for _, n := range t.List {
		if t.listed[n] {
			return fmt.Errorf("#%d is listed twice", n)
		}
		t.listed[n] = true
	}
	var saved *State
	var err error
	if !t.Reset { // -reset replaces the saved train, even an unreadable one
		if saved, err = loadState(t.State); err != nil {
			return err
		}
	}
	if saved != nil {
		if !slices.Equal(sortedCopy(saved.List), sortedCopy(t.List)) {
			return fmt.Errorf("%s holds a train for %v; pass the same list to resume or -reset to start over", t.State, saved.List)
		}
		t.state = saved
		t.logf("resuming train %v from %s", saved.Order, t.State)
	}
	prs := map[int]PR{}
	for _, n := range t.List {
		pr, err := t.GH.View(n)
		if err != nil {
			return err
		}
		if pr.Number != n {
			return fmt.Errorf("#%d: gh returned pull request #%d", n, pr.Number)
		}
		prs[n] = pr
	}
	if t.state == nil {
		var cs []candidate
		for i, n := range t.List {
			pr := prs[n]
			c := candidate{n: n, index: i, head: pr.HeadRefName, fork: pr.IsCrossRepository, base: pr.BaseRefName}
			if pr.State == "OPEN" {
				if c.files, err = t.GH.Files(n); err != nil {
					return err
				}
				c.ready = pr.MergeStateStatus == "CLEAN"
			}
			cs = append(cs, c)
		}
		ord, err := order(cs)
		if err != nil {
			return err
		}
		t.state = &State{List: slices.Clone(t.List), Order: ord, Parents: parents(cs), Merged: map[int]string{}, Notes: map[int]string{}, Update: map[int]time.Time{}}
		t.printPlan(cs, prs)
	}
	if t.state.Merged == nil {
		t.state.Merged = map[int]string{}
	}
	if t.state.Notes == nil {
		t.state.Notes = map[int]string{}
	}
	if t.state.Update == nil {
		t.state.Update = map[int]time.Time{}
	}
	if !t.Apply {
		return t.dryRun(prs)
	}
	if err := t.state.save(t.State); err != nil {
		return err
	}
	var conflicts []string
	held := map[int]int{} // pull request -> conflicted ancestor it waits for
	for _, n := range t.state.Order {
		if !t.listed[n] {
			return fmt.Errorf("state order names #%d, which is not listed", n) // never reached: lists are equal
		}
		if p, ok := held[n]; ok {
			conflicts = append(conflicts, fmt.Sprintf("#%d: stacked on #%d, which has a conflict; skipped", n, p))
			t.logf("#%d: skipped; it is stacked on conflicted #%d", n, p)
			t.state.Notes[n] = fmt.Sprintf("skipped: stacked on conflicted #%d", p)
			continue
		}
		err := t.one(n)
		if serr := t.state.save(t.State); serr != nil && err == nil {
			err = serr
		}
		var stop *Stop
		if errors.As(err, &stop) && strings.HasPrefix(stop.Reason, "conflicts with") {
			// Report and carry on: later pull requests that do not depend on
			// this one can still merge. Its stacked children cannot.
			conflicts = append(conflicts, stop.Error())
			t.logf("#%d: %s; continuing with the rest", n, stop.Reason)
			for _, d := range t.stackedOn(n) {
				held[d] = n
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("merged what could be merged; resolve these on their branches and rerun:\n%s", strings.Join(conflicts, "\n"))
	}
	t.logf("train complete: %v merged", t.state.Order)
	return nil
}

// stackedOn lists the train's pull requests stacked, directly or not, on n.
func (t *Train) stackedOn(n int) []int {
	var out []int
	for _, m := range t.state.Order {
		seen := map[int]bool{}
		for p, ok := t.state.Parents[m]; ok && !seen[p]; p, ok = t.state.Parents[p] {
			seen[p] = true
			if p == n {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

func sortedCopy(xs []int) []int { c := slices.Clone(xs); slices.Sort(c); return c }

func (t *Train) printPlan(cs []candidate, prs map[int]PR) {
	over := overlaps(cs)
	t.logf("merge order:")
	for i, n := range t.state.Order {
		pr := prs[n]
		line := fmt.Sprintf("  %d. #%d %s [%s %s]", i+1, n, pr.Title, pr.State, pr.MergeStateStatus)
		var shared []string
		for _, o := range t.state.Order {
			if files, ok := over[n][o]; ok {
				shared = append(shared, fmt.Sprintf("#%d (%s)", o, strings.Join(files, ", ")))
			}
		}
		if len(shared) > 0 {
			line += "\n     shares files with " + strings.Join(shared, "; ")
		}
		t.logf("%s", line)
	}
}

// dryRun reports what -apply would do first for each pull request, without
// changing anything on GitHub or writing the state file.
func (t *Train) dryRun(prs map[int]PR) error {
	t.logf("dry run (pass -apply to act):")
	for _, n := range t.state.Order {
		pr := prs[n]
		action, err := t.preflight(pr)
		var stop *Stop
		if errors.As(err, &stop) {
			msg := stop.Reason
			if stop.URL != "" {
				msg += " (" + stop.URL + ")"
			}
			t.logf("  #%d: would stop: %s", n, msg)
			continue
		}
		if err != nil {
			return err
		}
		t.logf("  #%d: %s", n, action)
	}
	return nil
}

// preflight is every check that needs no waiting. It returns what the next
// action would be, or why the pull request cannot go.
func (t *Train) preflight(pr PR) (string, error) {
	n := pr.Number
	switch pr.State {
	case "MERGED":
		return "already merged; skip", nil
	case "OPEN":
	default:
		return "", &Stop{PR: n, Reason: "is " + strings.ToLower(pr.State)}
	}
	if pr.IsDraft {
		return "", &Stop{PR: n, Reason: "is a draft"}
	}
	if pr.BaseRefName != t.Base {
		return "", &Stop{PR: n, Reason: fmt.Sprintf("targets %s, not %s; retarget it after its base merges", pr.BaseRefName, t.Base)}
	}
	if pr.ReviewDecision == "CHANGES_REQUESTED" {
		return "", &Stop{PR: n, Reason: "has changes requested", URL: pr.URL}
	}
	open, err := t.GH.UnresolvedThreads(n)
	if err != nil {
		return "", err
	}
	if open > 0 {
		return "", &Stop{PR: n, Reason: fmt.Sprintf("has %d unresolved review thread(s)", open), URL: pr.URL}
	}
	// A red check stops the train even on a conflicted pull request, so
	// read checks before reporting the conflict.
	checks, err := t.GH.Checks(n, false)
	if err != nil {
		return "", err
	}
	if red := firstRed(checks); red != nil {
		return "", redStop(n, red)
	}
	if pr.MergeStateStatus == "DIRTY" {
		return "", &Stop{PR: n, Reason: "conflicts with " + t.Base + "; resolve on its branch (the train does not)", URL: pr.URL}
	}
	switch pr.MergeStateStatus {
	case "BEHIND":
		return "behind " + t.Base + ": update branch (merge commit), wait for checks, merge", nil
	case "CLEAN", "HAS_HOOKS":
		if pending(checks) {
			return "up to date: wait for checks, merge", nil
		}
		return "up to date and green: merge", nil
	default:
		return fmt.Sprintf("%s: wait for checks, then merge if GitHub allows it", pr.MergeStateStatus), nil
	}
}

func redStop(n int, red *Check) *Stop {
	verb := "failed"
	if red.Bucket == "cancel" {
		verb = "was cancelled"
	}
	return &Stop{PR: n, Reason: fmt.Sprintf("check %q %s", red.Name, verb), URL: red.Link}
}

func firstRed(checks []Check) *Check {
	for i, c := range checks {
		if c.Bucket == "fail" || c.Bucket == "cancel" {
			return &checks[i]
		}
	}
	return nil
}

func pending(checks []Check) bool {
	for _, c := range checks {
		if c.Bucket == "pending" {
			return true
		}
	}
	return false
}

// one brings a single listed pull request to merged, or stops.
func (t *Train) one(n int) error {
	if !t.listed[n] {
		return fmt.Errorf("refusing #%d: not on the approved list", n)
	}
	t.deadline = t.Now().Add(t.Timeout)
	settle := 0 // re-reads granted to a BLOCKED state right after checks finish
	for {
		pr, err := t.GH.View(n)
		if err != nil {
			return err
		}
		if pr.State == "MERGED" {
			if t.state.Merged[n] == "" {
				t.state.Merged[n] = "merged outside this run"
			}
			t.logf("#%d: merged; skip", n)
			return nil
		}
		if _, err := t.preflight(pr); err != nil {
			t.state.Notes[n] = err.Error()
			return err
		}
		switch pr.MergeStateStatus {
		case "BEHIND":
			t.logf("#%d: behind %s; updating branch with a merge commit", n, t.Base)
			if err := t.GH.UpdateBranch(n); err != nil {
				// GitHub refuses an update that conflicts; say so plainly.
				if again, verr := t.GH.View(n); verr == nil && again.MergeStateStatus == "DIRTY" {
					return &Stop{PR: n, Reason: "conflicts with " + t.Base + "; resolve on its branch (the train does not)", URL: pr.URL}
				}
				return err
			}
			t.state.Update[n] = t.Now()
			if err := t.state.save(t.State); err != nil {
				return err
			}
			if err := t.waitHead(n, pr.HeadRefOid); err != nil {
				return err
			}
			continue
		case "UNKNOWN", "":
			if err := t.wait(n, "GitHub to compute mergeability"); err != nil {
				return err
			}
			continue
		}
		// CLEAN, HAS_HOOKS, BLOCKED, UNSTABLE: settle every check first.
		// These two reads decide the merge: a check that went red since
		// preflight stops the train, and anything still pending waits.
		checks, err := t.GH.Checks(n, false)
		if err != nil {
			return err
		}
		if red := firstRed(checks); red != nil {
			return redStop(n, red)
		}
		if pending(checks) || len(checks) == 0 {
			if err := t.wait(n, "checks"); err != nil {
				return err
			}
			continue
		}
		required, err := t.GH.Checks(n, true)
		if err != nil {
			return err
		}
		if red := firstRed(required); red != nil {
			return redStop(n, red)
		}
		if pending(required) {
			if err := t.wait(n, "required checks"); err != nil {
				return err
			}
			continue
		}
		if (pr.MergeStateStatus == "BLOCKED" || pr.MergeStateStatus == "UNSTABLE") && settle < blockedSettle {
			settle++ // GitHub can lag the last check by a poll or two
			if err := t.wait(n, "GitHub to settle the merge state"); err != nil {
				return err
			}
			continue
		}
		if pr.MergeStateStatus == "BLOCKED" || pr.MergeStateStatus == "UNSTABLE" {
			// Every check is green, yet protection still blocks: a missing
			// approval or a required check that never reported. Waiting
			// cannot fix that and the train never overrides it.
			return &Stop{PR: n, Reason: fmt.Sprintf("branch protection blocks the merge (%s) with %d required check(s) green; it needs the owner, not the train", pr.MergeStateStatus, len(required)), URL: pr.URL}
		}
		t.logf("#%d: %d check(s) green (%d required); merging %s with a merge commit", n, len(checks), len(required), short(pr.HeadRefOid))
		if err := t.GH.Merge(n, pr.HeadRefOid); err != nil {
			return err
		}
		after, err := t.GH.View(n)
		if err != nil {
			return err
		}
		if after.State != "MERGED" {
			return &Stop{PR: n, Reason: "gh reported the merge but the pull request is " + after.State, URL: pr.URL}
		}
		t.state.Merged[n] = pr.HeadRefOid
		t.state.Notes[n] = "merged"
		t.logf("#%d: merged", n)
		return nil
	}
}

// blockedSettle polls tell GitHub lag from a real block.
const blockedSettle = 2

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func (t *Train) wait(n int, what string) error {
	if t.Now().After(t.deadline) {
		return &Stop{PR: n, Reason: fmt.Sprintf("timed out after %s waiting for %s", t.Timeout, what)}
	}
	t.logf("#%d: waiting for %s", n, what)
	t.Sleep(t.Poll)
	return nil
}

// waitHead waits for the update to show up as a new head commit, so the
// checks read next belong to it and not to the old head.
func (t *Train) waitHead(n int, old string) error {
	for {
		pr, err := t.GH.View(n)
		if err != nil {
			return err
		}
		if pr.HeadRefOid != old || pr.State != "OPEN" {
			return nil
		}
		if err := t.wait(n, "the updated head"); err != nil {
			return err
		}
	}
}

// statePath is .git/merge-train.json in the main repository, shared by
// its worktrees.
func statePath() (string, error) {
	out, err := gitOutput("rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(out)
	if !filepath.IsAbs(dir) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(wd, dir)
	}
	return filepath.Join(dir, "merge-train.json"), nil
}
