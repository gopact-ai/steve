// Package planner turns a goal into a plan, and revises one when the world
// turns out differently than assumed.
//
// There is deliberately more than one way to produce a plan — a workflow the
// operator declared, a rule that matches capabilities, a model that
// decomposes an open-ended goal — and exactly one way to execute one. The
// interface here is the seam: every planner emits the same validated Plan,
// and the executor never learns which kind made it.
package planner

import (
	"context"

	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/roster"
)

// Request is what a planner is given. It is the same for the first plan and
// for every revision: revising is planning with more known.
type Request struct {
	Goal   string
	TaskID string
	// ProjectID is the task's project: what the plan is about, and where a
	// planning session that wants to look around is opened.
	ProjectID string
	// Current is the plan being revised; zero for a first plan.
	Current plan.Plan
	// Trigger is what prompted this call — a failed step, a finding, a node
	// that vanished, a person stepping in. It becomes the revision's
	// Because, so it must read as an explanation, not a code.
	Trigger string
	// Roster is who is available right now, with reasons for whoever is not.
	Roster []roster.Candidate
	// Budget left on the task, so a planner can prefer a cheaper shape when
	// there is not much room to work in.
	TurnsLeft int
}

// Planner produces a plan or a revision.
type Planner interface {
	// Name identifies the planner in the revision record. Planning is an
	// attributable act: a plan nobody can trace back to its author cannot
	// be argued with later.
	Name() string
	Plan(ctx context.Context, req Request) (plan.Plan, error)
}
