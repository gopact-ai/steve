package conversationapi

import "time"

type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// Seq is gopact's per-run sequence. Delivery is at-least-once, so a
	// consumer that cares about exactly-once dedupes on (RunID, Seq).
	Seq    int64  `json:"seq,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	PlanID string `json:"plan_id,omitempty"`
	StepID string `json:"step_id,omitempty"`
	State  string `json:"state,omitempty"`
	// Conversation and Text carry the console's traffic: what was sent
	// from the page, what came back, and milestones agents posted. A run
	// event is stamped with the conversation its task came from, so a
	// page can follow one conversation's work.
	Conversation string `json:"conversation,omitempty"`
	Text         string `json:"text,omitempty"`
	Format       string `json:"format,omitempty"`
	// Progress is what an agent is doing right now: console.progress for
	// a chat turn, step.progress for a plan step.
	Progress *Progress `json:"progress,omitempty"`
	// Step is the child's snapshot; console.step also names its owning reply.
	Step *StepProcess `json:"step,omitempty"`
	// ReplyID names the console line a console.* event is about.
	ReplyID    string `json:"reply_id,omitempty"`
	ExchangeID string `json:"exchange_id,omitempty"`
	// Silent marks a console line the transcript does not draw, so a live
	// page hides it the moment it arrives rather than after its next read.
	Silent bool   `json:"silent,omitempty"`
	Title  string `json:"title,omitempty"`
	Rev    int    `json:"rev,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Data carries an observation's facts apart from its sentence, so a
	// live page can say them the same way the history page does.
	Data map[string]string `json:"data,omitempty"`
	// Cursor is the event's place in the model's stream, in publication
	// order. The stream carries it as the event's id rather than in the
	// event itself.
	Cursor uint64 `json:"-"`
}
