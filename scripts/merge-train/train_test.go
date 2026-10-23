package main

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOrderPutsDisjointFirstAndKeepsOverlapsInListOrder(t *testing.T) {
	cs := []candidate{
		{n: 10, index: 0, head: "a", base: "master", files: []string{"x.go", "shared.go"}},
		{n: 11, index: 1, head: "b", base: "master", files: []string{"solo.go"}},
		{n: 12, index: 2, head: "c", base: "master", files: []string{"shared.go"}},
		{n: 13, index: 3, head: "d", base: "master", files: []string{"alone.md"}},
	}
	got, err := order(cs)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{11, 13, 10, 12}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestOrderMergesAReadyPullRequestFirstWhenFree(t *testing.T) {
	cs := []candidate{
		{n: 1, index: 0, head: "a", base: "master", files: []string{"a.go"}},
		{n: 2, index: 1, head: "b", base: "master", files: []string{"b.go"}, ready: true},
		{n: 3, index: 2, head: "c", base: "master", files: []string{"b.go"}},
	}
	got, err := order(cs)
	if err != nil {
		t.Fatal(err)
	}
	// #2 costs no CI run now; #3 must still follow it (shared file, list order).
	if want := []int{2, 1, 3}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestOrderPutsAStackedPullRequestAfterItsBase(t *testing.T) {
	cs := []candidate{
		{n: 20, index: 0, head: "child", base: "parent", files: []string{"p.go", "c.go"}},
		{n: 21, index: 1, head: "parent", base: "master", files: []string{"p.go"}},
		{n: 22, index: 2, head: "other", base: "master", files: []string{"o.go"}},
	}
	got, err := order(cs)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{22, 21, 20}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if _, err := order([]candidate{{n: 1, head: "a", base: "b"}, {n: 2, index: 1, head: "b", base: "a"}}); err == nil {
		t.Fatal("a stacking cycle must be an error")
	}
}

func calls(w *world, verb string) [][]string {
	var out [][]string
	for _, c := range w.Calls {
		if len(c) >= 2 && c[0] == "pr" && c[1] == verb {
			out = append(out, c)
		}
	}
	return out
}

func mutations(w *world) [][]string { return append(calls(w, "merge"), calls(w, "update-branch")...) }

func TestDryRunChangesNothing(t *testing.T) {
	h := newHarness(t, open(1, "BEHIND", "a.go"), open(2, "CLEAN", "b.go"))
	if err := h.train(false, 1, 2).Run(); err != nil {
		t.Fatal(err)
	}
	if m := mutations(h.read()); len(m) != 0 {
		t.Fatalf("dry run called %v", m)
	}
	if _, err := os.Stat(h.state); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote %s", h.state)
	}
	for _, want := range []string{"1. #2", "2. #1", "#1: behind master: update branch", "#2: up to date and green: merge", "pass -apply"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, h.out.String())
		}
	}
}

func TestApplyUpdatesBehindBranchesWaitsAndMergesInOrder(t *testing.T) {
	h := newHarness(t, open(1, "CLEAN", "a.go"), open(2, "CLEAN", "b.go"), open(3, "BEHIND", "c.go"))
	if err := h.train(true, 1, 2, 3).Run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	w := h.read()
	var merged []string
	for _, c := range calls(w, "merge") {
		merged = append(merged, c[2])
	}
	if want := []string{"1", "2", "3"}; !slices.Equal(merged, want) {
		t.Fatalf("merged %v, want %v", merged, want)
	}
	// #1 was up to date; #2 fell behind when #1 merged; #3 was behind.
	if u := calls(w, "update-branch"); len(u) != 2 {
		t.Fatalf("updates = %v, want one each for #2 and #3", u)
	}
	for _, n := range []string{"1", "2", "3"} {
		if w.PRs[n].PR.State != "MERGED" {
			t.Errorf("#%s is %s", n, w.PRs[n].PR.State)
		}
	}
	if !strings.Contains(h.out.String(), "waiting for checks") {
		t.Errorf("an updated branch must wait for its new checks:\n%s", h.out.String())
	}
}

func TestSafetyFlagsOnEveryMutation(t *testing.T) {
	h := newHarness(t, open(1, "BEHIND", "a.go"), open(2, "BEHIND", "b.go"))
	if err := h.train(true, 1, 2).Run(); err != nil {
		t.Fatal(err)
	}
	w := h.read()
	for _, c := range calls(w, "merge") {
		if !has(c, "--merge") || !has(c, "--match-head-commit") || has(c, "--admin") || has(c, "--auto") || has(c, "--squash") || has(c, "--rebase") {
			t.Errorf("merge call %v must be a merge commit of the checked head, without --admin/--auto", c)
		}
	}
	for _, c := range calls(w, "update-branch") {
		if has(c, "--rebase") {
			t.Errorf("update %v must merge, not rebase", c)
		}
	}
	if len(calls(w, "merge")) != 2 {
		t.Fatalf("merges = %v", calls(w, "merge"))
	}
}

func TestNeverTouchesAPullRequestOffTheList(t *testing.T) {
	// #9 is ready and stacked on #1's branch, but the owner did not list it.
	unlisted := open(9, "CLEAN", "a.go")
	unlisted.PR.BaseRefName = "branch-1"
	h := newHarness(t, open(1, "CLEAN", "a.go"), unlisted)
	if err := h.train(true, 1).Run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.read().Calls {
		if slices.Contains(c, "9") {
			t.Fatalf("called gh about unlisted #9: %v", c)
		}
	}
	if err := (&Train{GH: &cli{bin: "false"}, List: []int{1}, listed: map[int]bool{1: true}}).one(9); err == nil || !strings.Contains(err.Error(), "not on the approved list") {
		t.Fatalf("one(unlisted) = %v", err)
	}
}

func TestStopsOnFirstRedCheckWithItsJobURL(t *testing.T) {
	red := open(2, "BEHIND", "b.go")
	red.UpdateChecks = green("console", "desktop", "macos", "fleet")
	red.UpdateChecks = append(red.UpdateChecks, Check{Name: "test", Bucket: "fail", State: "FAILURE", Link: "https://ci.example/run/7/job/42"})
	h := newHarness(t, open(1, "BEHIND", "a.go"), red, open(3, "BEHIND", "c.go"))
	err := h.train(true, 1, 2, 3).Run()
	var stop *Stop
	if !errors.As(err, &stop) || stop.PR != 2 || stop.URL != "https://ci.example/run/7/job/42" || !strings.Contains(stop.Reason, `"test" failed`) {
		t.Fatalf("err = %v", err)
	}
	w := h.read()
	if w.PRs["2"].PR.State == "MERGED" || w.PRs["3"].PR.State == "MERGED" || len(calls(w, "update-branch")) != 2 {
		t.Fatalf("must stop at #2 without touching #3: updates %v merges %v", calls(w, "update-branch"), calls(w, "merge"))
	}
}

func TestStopsBeforeMergingWithUnresolvedThreadsOrRequestedChanges(t *testing.T) {
	threads := open(1, "CLEAN", "a.go")
	threads.Threads = 2
	h := newHarness(t, threads)
	err := h.train(true, 1).Run()
	if err == nil || !strings.Contains(err.Error(), "2 unresolved review thread(s)") {
		t.Fatalf("err = %v", err)
	}
	changes := open(1, "CLEAN", "a.go")
	changes.PR.ReviewDecision = "CHANGES_REQUESTED"
	h2 := newHarness(t, changes)
	if err := h2.train(true, 1).Run(); err == nil || !strings.Contains(err.Error(), "changes requested") {
		t.Fatalf("err = %v", err)
	}
	draft := open(1, "CLEAN", "a.go")
	draft.PR.IsDraft = true
	h3 := newHarness(t, draft)
	if err := h3.train(true, 1).Run(); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("err = %v", err)
	}
	for _, x := range []*harness{h, h2, h3} {
		if m := mutations(x.read()); len(m) != 0 {
			t.Fatalf("mutated: %v", m)
		}
	}
}

func TestDoesNotOverrideBranchProtection(t *testing.T) {
	blocked := open(1, "BLOCKED", "a.go") // e.g. a missing approval; checks green
	h := newHarness(t, blocked)
	err := h.train(true, 1).Run()
	if err == nil || !strings.Contains(err.Error(), "branch protection blocks") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.out.String(), "waiting for GitHub to settle") {
		t.Fatalf("a fresh BLOCKED state gets a short grace first:\n%s", h.out.String())
	}
	if m := calls(h.read(), "merge"); len(m) != 0 {
		t.Fatalf("merged a blocked pull request: %v", m)
	}
}

func TestReportsConflictsAndMergesTheRest(t *testing.T) {
	conflicted := open(1, "BEHIND", "shared.go")
	conflicted.UpdateConflicts = true
	refused := open(4, "BEHIND", "fourth.go") // gh exits non-zero on the update
	refused.UpdateRefused = true
	h := newHarness(t, conflicted, open(2, "BEHIND", "other.go"), open(3, "DIRTY", "third.go"), refused)
	err := h.train(true, 1, 2, 3, 4).Run()
	for _, want := range []string{"#1: conflicts with master", "#3: conflicts with master", "#4: conflicts with master"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
	w := h.read()
	if w.PRs["2"].PR.State != "MERGED" || w.PRs["1"].PR.State == "MERGED" || w.PRs["3"].PR.State == "MERGED" || w.PRs["4"].PR.State == "MERGED" {
		t.Fatalf("states: 1=%s 2=%s 3=%s 4=%s", w.PRs["1"].PR.State, w.PRs["2"].PR.State, w.PRs["3"].PR.State, w.PRs["4"].PR.State)
	}
	// The train reports conflicts; it never edits a branch to resolve one.
	for _, c := range calls(w, "update-branch") {
		if c[2] == "3" {
			t.Fatalf("updated already-conflicting #3: %v", c)
		}
	}
}

func TestResumeSkipsMergedAndKeepsTheOrder(t *testing.T) {
	red := open(2, "BEHIND", "b.go")
	red.UpdateChecks = append(green("console", "desktop", "macos", "fleet"), Check{Name: "test", Bucket: "fail", Link: "https://ci.example/job/1"})
	h := newHarness(t, open(1, "CLEAN", "a.go"), red, open(3, "BEHIND", "c.go"))
	if err := h.train(true, 1, 2, 3).Run(); err == nil {
		t.Fatal("first run must stop at the red check")
	}
	st, err := loadState(h.state)
	if err != nil || st == nil || st.Merged[1] == "" || st.Merged[2] != "" {
		t.Fatalf("state after stop = %+v, %v", st, err)
	}
	// The author fixes #2; its new head goes green.
	w := h.read()
	w.PRs["2"].Checks = green(requiredChecks...)
	w.PRs["2"].PR.HeadRefOid = "sha2-fixed"
	w.Calls = nil
	h.write(w)
	// The same list in another order resumes the saved order.
	if err := h.train(true, 3, 2, 1).Run(); err != nil {
		t.Fatalf("resume: %v\n%s", err, h.out.String())
	}
	w = h.read()
	var merged []string
	for _, c := range calls(w, "merge") {
		merged = append(merged, c[2])
	}
	if !slices.Equal(merged, []string{"2", "3"}) {
		t.Fatalf("resume merged %v, want [2 3] (and never #1 again)", merged)
	}
	if !strings.Contains(h.out.String(), "resuming train") || !strings.Contains(h.out.String(), "#1: merged; skip") {
		t.Fatalf("resume output:\n%s", h.out.String())
	}
	// A third run is a no-op: everything is merged.
	w.Calls = nil
	h.write(w)
	if err := h.train(true, 1, 2, 3).Run(); err != nil {
		t.Fatal(err)
	}
	if m := mutations(h.read()); len(m) != 0 {
		t.Fatalf("idempotent rerun mutated: %v", m)
	}
}

func TestResumeRefusesADifferentListUnlessReset(t *testing.T) {
	h := newHarness(t, open(1, "CLEAN", "a.go"), open(2, "CLEAN", "b.go"))
	if err := (&State{List: []int{1}, Order: []int{1}}).save(h.state); err != nil {
		t.Fatal(err)
	}
	if err := h.train(true, 1, 2).Run(); err == nil || !strings.Contains(err.Error(), "-reset") {
		t.Fatalf("err = %v", err)
	}
	if m := mutations(h.read()); len(m) != 0 {
		t.Fatalf("mutated on a list mismatch: %v", m)
	}
	tr := h.train(true, 1, 2)
	tr.Reset = true
	if err := tr.Run(); err != nil {
		t.Fatal(err)
	}
	if len(calls(h.read(), "merge")) != 2 {
		t.Fatal("reset run must merge both")
	}
}

func TestMergedOutsideTheTrainIsSkipped(t *testing.T) {
	done := open(1, "CLEAN", "a.go")
	done.PR.State = "MERGED"
	h := newHarness(t, done, open(2, "CLEAN", "b.go"))
	if err := h.train(true, 1, 2).Run(); err != nil {
		t.Fatal(err)
	}
	m := calls(h.read(), "merge")
	if len(m) != 1 || m[0][2] != "2" {
		t.Fatalf("merges = %v", m)
	}
}

func TestParseList(t *testing.T) {
	got, err := parseList([]string{"#369", "370,371"})
	if err != nil || !slices.Equal(got, []int{369, 370, 371}) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][]string{nil, {"abc"}, {"0"}, {"-3"}} {
		if _, err := parseList(bad); err == nil {
			t.Errorf("parseList(%q) must fail", bad)
		}
	}
	if err := (&Train{List: []int{1, 1}}).Run(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate list: %v", err)
	}
}

func TestCLIAgainstFakeGHBinary(t *testing.T) {
	// One end-to-end run through a real gh subprocess (the test binary):
	// exit codes, stdout JSON and flags as gh would see them.
	// Kept to one ready pull request: each call is a process start.
	h := newHarness(t, open(1, "CLEAN", "a.go"))
	if err := h.subprocess(h.train(true, 1)).Run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	w := h.read()
	if w.PRs["1"].PR.State != "MERGED" {
		t.Fatalf("state %s", w.PRs["1"].PR.State)
	}
	if m := calls(w, "merge"); len(m) != 1 || !has(m[0], "--match-head-commit") {
		t.Fatalf("merge calls %v", m)
	}
}

func TestDiffNamesKeepSpacesInsidePaths(t *testing.T) {
	got := diffNames([]byte("docs/design notes.md\r\ndocs/release notes.md\n\nweb/a b/c.ts\n"))
	want := []string{"docs/design notes.md", "docs/release notes.md", "web/a b/c.ts"}
	if !slices.Equal(got, want) {
		t.Fatalf("diffNames = %q, want %q", got, want)
	}
	// Two pull requests that only share a word of a path are disjoint, so
	// the CLEAN one still goes first.
	cs := []candidate{
		{n: 1, index: 0, head: "a", base: "master", files: diffNames([]byte("docs/design notes.md\n"))},
		{n: 2, index: 1, head: "b", base: "master", files: diffNames([]byte("docs/release notes.md\n")), ready: true},
	}
	if o := overlaps(cs); len(o) != 0 {
		t.Fatalf("fictitious overlap %v", o)
	}
	h := newHarness(t, open(1, "BEHIND", "docs/design notes.md"), open(2, "CLEAN", "docs/release notes.md"))
	if err := h.train(false, 1, 2).Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "1. #2") || strings.Contains(h.out.String(), "shares files") {
		t.Fatalf("plan:\n%s", h.out.String())
	}
}

func TestForkHeadsAreNeverStackParents(t *testing.T) {
	// #1 comes from a fork's "master"; #2 and #3 target upstream master and
	// overlap. Without fork identity #1 looked like their base.
	fork := open(1, "BEHIND", "x.go")
	fork.PR.HeadRefName, fork.PR.IsCrossRepository = "master", true
	behind, clean := open(2, "BEHIND", "shared.go"), open(3, "CLEAN", "shared.go", "y.go")
	h := newHarness(t, fork, behind, clean)
	if err := h.train(false, 1, 2, 3).Run(); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	if !strings.Contains(out, "1. #1") || !strings.Contains(out, "2. #2") || !strings.Contains(out, "3. #3") {
		t.Fatalf("order changed by a fork head:\n%s", out)
	}
	if p := parents([]candidate{{n: 1, head: "master", fork: true, base: "master"}, {n: 2, index: 1, head: "b", base: "master"}}); len(p) != 0 {
		t.Fatalf("fork head became a parent: %v", p)
	}
	// A same-repository head is still a parent.
	if p := parents([]candidate{{n: 1, head: "feature", base: "master"}, {n: 2, index: 1, head: "b", base: "feature"}}); p[2] != 1 {
		t.Fatalf("stack lost: %v", p)
	}
}

func TestResetReplacesAnUnreadableState(t *testing.T) {
	h := newHarness(t, open(1, "CLEAN", "a.go"))
	if err := os.WriteFile(h.state, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.train(true, 1).Run(); err == nil || !strings.Contains(err.Error(), "-reset") {
		t.Fatalf("without -reset: %v", err)
	}
	tr := h.train(true, 1)
	tr.Reset = true
	if err := tr.Run(); err != nil {
		t.Fatalf("-reset: %v", err)
	}
	if st, err := loadState(h.state); err != nil || st == nil || st.Merged[1] == "" {
		t.Fatalf("state after reset = %+v, %v", st, err)
	}
}

func TestDescendantsOfAConflictAreSkippedAndTheRestMerge(t *testing.T) {
	parent := open(1, "DIRTY", "p.go")
	child := open(2, "CLEAN", "p.go", "c.go")
	child.PR.BaseRefName = "branch-1"
	grandchild := open(3, "CLEAN", "g.go")
	grandchild.PR.BaseRefName = "branch-2"
	other := open(4, "BEHIND", "o.go")
	h := newHarness(t, parent, child, grandchild, other)
	err := h.train(true, 1, 2, 3, 4).Run()
	for _, want := range []string{"#1: conflicts with master", "#2: stacked on #1", "#3: stacked on #1"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
	w := h.read()
	if w.PRs["4"].PR.State != "MERGED" {
		t.Fatalf("an unrelated pull request must still merge; out:\n%s", h.out.String())
	}
	for _, c := range h.read().Calls {
		if len(c) > 2 && c[0] == "pr" && (c[1] == "merge" || c[1] == "update-branch") && (c[2] == "2" || c[2] == "3") {
			t.Fatalf("acted on a descendant of the conflict: %v", c)
		}
	}
}

func TestConflictedPullRequestWithARedCheckStopsTheTrain(t *testing.T) {
	dirty := open(1, "DIRTY", "a.go")
	dirty.Checks = append(green("console", "desktop", "macos", "fleet"), Check{Name: "test", Bucket: "fail", Link: "https://ci.example/job/9"})
	h := newHarness(t, dirty, open(2, "BEHIND", "b.go"))
	err := h.train(true, 1, 2).Run()
	var stop *Stop
	if !errors.As(err, &stop) || stop.PR != 1 || stop.URL != "https://ci.example/job/9" || strings.Contains(err.Error(), "merged what could be merged") {
		t.Fatalf("err = %v", err)
	}
	if m := mutations(h.read()); len(m) != 0 {
		t.Fatalf("carried on after a red check: %v", m)
	}
}

func TestACheckThatFailsBetweenReadsIsNeverMerged(t *testing.T) {
	// Preflight sees an optional job pending; by the decisive read it has
	// failed. Protection would not block it (it is not required).
	pr := open(1, "CLEAN", "a.go")
	pr.Checks = append(green(requiredChecks...), Check{Name: "lint", Bucket: "pending", Link: "https://ci.example/job/lint"})
	pr.FlipAfter, pr.Flip = 1, append(green(requiredChecks...), Check{Name: "lint", Bucket: "fail", Link: "https://ci.example/job/lint"})
	h := newHarness(t, pr)
	err := h.train(true, 1).Run()
	var stop *Stop
	if !errors.As(err, &stop) || stop.URL != "https://ci.example/job/lint" {
		t.Fatalf("err = %v\n%s", err, h.out.String())
	}
	if m := calls(h.read(), "merge"); len(m) != 0 {
		t.Fatalf("merged after a check went red: %v", m)
	}
	// A cancelled required check read last is a stop too.
	req := open(2, "CLEAN", "b.go")
	req.FlipAfter, req.Flip = 2, append(green("console", "desktop", "macos", "fleet"), Check{Name: "test", Bucket: "cancel", Link: "https://ci.example/job/test"})
	h2 := newHarness(t, req)
	if err := h2.train(true, 2).Run(); !errors.As(err, &stop) || !strings.Contains(stop.Reason, "cancelled") {
		t.Fatalf("err = %v", err)
	}
	if m := calls(h2.read(), "merge"); len(m) != 0 {
		t.Fatalf("merged with a cancelled required check: %v", m)
	}
}

func TestPendingRequiredChecksAreWaitedFor(t *testing.T) {
	pr := open(1, "CLEAN", "a.go")
	// From the third check read on, "test" (required) is pending until the
	// train has waited once; it must not merge on that read.
	pr.FlipAfter, pr.Flip = 2, []Check{{Name: "test", Bucket: "pending"}, {Name: "console", Bucket: "pass"}, {Name: "desktop", Bucket: "pass"}, {Name: "macos", Bucket: "pass"}, {Name: "fleet", Bucket: "pass"}}
	h := newHarness(t, pr)
	w := h.read()
	w.PRs["1"].Required = requiredChecks
	h.write(w)
	tr := h.train(true, 1)
	reads := 0
	tr.Sleep = func(time.Duration) {
		// The pending required check finishes while the train waits.
		reads++
		w := h.read()
		w.PRs["1"].Checks = green(requiredChecks...)
		h.write(w)
	}
	if err := tr.Run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if reads == 0 || !strings.Contains(h.out.String(), "waiting for checks") && !strings.Contains(h.out.String(), "waiting for required checks") {
		t.Fatalf("must wait on a pending required check:\n%s", h.out.String())
	}
	if len(calls(h.read(), "merge")) != 1 {
		t.Fatal("must merge once the required check passes")
	}
}
