package plan

import (
	"path/filepath"
	"strings"
	"testing"
)

func step(id, goal string, needs ...string) Step {
	return Step{
		ID: id, Goal: goal, Needs: needs, Requires: []string{"any"},
		State: StepPending, Verify: &Verify{Kind: VerifyNone, Why: "test fixture"},
	}
}

func TestValidateRefusesWhatCannotRun(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{"empty", Plan{}, "no steps"},
		{"duplicate id", Plan{Steps: []Step{step("a", "x"), step("a", "y")}}, "duplicate step id"},
		{"dangling dep", Plan{Steps: []Step{step("a", "x", "ghost")}}, "unknown step"},
		{"self dep", Plan{Steps: []Step{step("a", "x", "a")}}, "depends on itself"},
		{
			"cycle",
			Plan{Steps: []Step{step("a", "x", "c"), step("b", "y", "a"), step("c", "z", "b")}},
			"cycle",
		},
		{
			"dependency listed twice",
			Plan{Steps: []Step{step("a", "x"), {
				ID: "m", Goal: "merge", Requires: []string{"any"}, State: StepPending,
				Needs: []string{"a"}, Merge: []string{"a"},
				Verify: &Verify{Kind: VerifyNone, Why: "w"},
			}}},
			"more than once",
		},
		{
			"unplaceable",
			Plan{Steps: []Step{{ID: "a", Goal: "x", State: StepPending, Verify: &Verify{Kind: VerifyNone, Why: "w"}}}},
			"neither an agent nor any requirement",
		},
		{
			"no verify decision",
			Plan{Steps: []Step{{ID: "a", Goal: "x", Requires: []string{"any"}, State: StepPending}}},
			"does not say how it is verified",
		},
		{
			"silent opt-out",
			Plan{Steps: []Step{{ID: "a", Goal: "x", Requires: []string{"any"}, Verify: &Verify{Kind: VerifyNone}}}},
			"without saying why",
		},
		{
			"command verify with no command",
			Plan{Steps: []Step{{ID: "a", Goal: "x", Requires: []string{"any"}, Verify: &Verify{Kind: VerifyCommand}}}},
			"names none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.plan)
			if err == nil {
				t.Fatalf("plan was accepted; expected %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestReadyIsTheWholeScheduler: fan-out and fan-in are not special cases,
// they are what a DAG does when you ask which steps can run now.
func TestReadyIsTheWholeScheduler(t *testing.T) {
	p := Plan{Steps: []Step{
		step("build", "compile"),
		step("test-a", "unit", "build"),
		step("test-b", "integration", "build"),
		{
			ID: "ship", Goal: "release", Merge: []string{"test-a", "test-b"},
			Requires: []string{"prod"}, State: StepPending,
			Verify: &Verify{Kind: VerifyNone, Why: "fixture"},
		},
	}}
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}

	ready := ids(p.Ready())
	if len(ready) != 1 || ready[0] != "build" {
		t.Fatalf("first wave = %v, want [build]", ready)
	}

	p.Steps[0].State = StepDone
	ready = ids(p.Ready())
	if len(ready) != 2 || ready[0] != "test-a" || ready[1] != "test-b" {
		t.Fatalf("fan-out wave = %v, want both tests ready at once", ready)
	}

	// One branch finishing is not enough: the merge waits for both.
	p.Steps[1].State = StepDone
	if ready := ids(p.Ready()); len(ready) != 1 || ready[0] != "test-b" {
		t.Fatalf("half-finished fan-out = %v, merge should still be blocked", ready)
	}
	p.Steps[2].State = StepDone
	if ready := ids(p.Ready()); len(ready) != 1 || ready[0] != "ship" {
		t.Fatalf("fan-in wave = %v, want [ship]", ready)
	}
	p.Steps[3].State = StepDone
	if !p.Complete() {
		t.Fatal("plan should be complete")
	}
}

// TestRevisionsAccumulate: the point of persisting plans is being able to
// answer "what changed and why" after the fact.
func TestRevisionsAccumulate(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(Plan{
		TaskID: "12", Goal: "ship it", By: "rule",
		Steps: []Step{step("a", "do the thing")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Rev != 1 || created.Because == "" {
		t.Fatalf("first revision = %+v", created)
	}

	revised, err := store.Revise(created.ID,
		[]Step{step("a", "do the thing"), step("b", "do it differently", "a")},
		"rule", "step a failed on node-b")
	if err != nil {
		t.Fatal(err)
	}
	if revised.Rev != 2 || revised.Because != "step a failed on node-b" {
		t.Fatalf("second revision = %+v", revised)
	}

	history := store.Revisions(created.ID)
	if len(history) != 2 || history[0].Rev != 1 || history[1].Rev != 2 {
		t.Fatalf("history = %d revisions", len(history))
	}
	if len(history[0].Steps) != 1 {
		t.Fatal("revising mutated the earlier revision")
	}

	// A revision with no stated cause is refused: an unexplained change
	// reads afterwards as if the plan always said that.
	if _, err := store.Revise(created.ID, revised.Steps, "rule", ""); err == nil {
		t.Fatal("a revision without a reason was accepted")
	}

	// An invalid revision must not land, and must not disturb the current one.
	if _, err := store.Revise(created.ID, []Step{step("x", "y", "ghost")}, "rule", "bad"); err == nil {
		t.Fatal("an invalid revision was accepted")
	}
	if latest, _ := store.Latest(created.ID); latest.Rev != 2 {
		t.Fatalf("a rejected revision changed the current plan to rev %d", latest.Rev)
	}
}

func TestStoreSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(Plan{TaskID: "7", Goal: "g", By: "rule", Steps: []Step{step("a", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	found, ok := reopened.ForTask("7")
	if !ok || found.ID != created.ID || found.Goal != "g" {
		t.Fatalf("plan did not survive reopen: %+v %v", found, ok)
	}
}

func ids(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.ID
	}
	return out
}
