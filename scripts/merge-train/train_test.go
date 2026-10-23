package main

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
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
