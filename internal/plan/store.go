package plan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// Store persists plans and their revisions with the same durable-replace
// discipline as the task store: write a temp file, fsync, rename, fsync the
// directory. A plan that survives a crash is what lets a task resume where
// it got to instead of starting over.
type Store struct {
	doc  ledger.Doc
	mu   sync.Mutex
	data data
	now  func() time.Time
}

type data struct {
	NextID int `json:"next_id"`
	// Plans holds every revision, newest last, keyed by plan id. Revisions
	// are appended rather than replaced: "what changed and why" is only
	// answerable if the earlier answer is still there.
	Plans map[string][]Plan `json:"plans"`
	// ByTask maps a task to its plan id, so the executor can find the plan
	// for work that arrived through the chat.
	ByTask map[string]string `json:"by_task"`
}

// Open keeps the store in one JSON file; the gateway opens the ledger.
func Open(path string) (*Store, error) {
	return openWith(&ledger.FileDocument{Path: path})
}

// OpenLedger keeps the store in the ledger, importing a legacy file once.
func OpenLedger(l *ledger.Ledger, legacy string) (*Store, error) {
	doc := l.Document("plans")
	if _, err := doc.Import(legacy); err != nil {
		return nil, err
	}
	return openWith(doc)
}

func openWith(doc ledger.Doc) (*Store, error) {
	s := &Store{
		doc: doc, now: time.Now,
		data: data{NextID: 1, Plans: map[string][]Plan{}, ByTask: map[string]string{}},
	}
	raw, ok, err := doc.Load()
	if err != nil {
		return nil, fmt.Errorf("read plans: %w", err)
	}
	if !ok || len(raw) == 0 {
		return s, nil
	}
	var loaded data
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("decode plans: %w", err)
	}
	if loaded.Plans == nil {
		loaded.Plans = map[string][]Plan{}
	}
	if loaded.ByTask == nil {
		loaded.ByTask = map[string]string{}
	}
	if loaded.NextID < 1 {
		loaded.NextID = 1
	}
	s.data = loaded
	return s, nil
}

// Create stores revision 1. The plan is validated first: an invalid plan
// must never reach the executor, where a cycle would simply stall.
func (s *Store) Create(p Plan) (Plan, error) {
	if err := Validate(p); err != nil {
		return Plan{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p.ID = strconv.Itoa(s.data.NextID)
	p.Rev = 1
	p.CreatedAt = s.now()
	if p.Because == "" {
		p.Because = "initial plan"
	}
	next := s.clone()
	next.NextID = s.data.NextID + 1
	next.Plans[p.ID] = []Plan{p}
	if p.TaskID != "" {
		next.ByTask[p.TaskID] = p.ID
	}
	if err := s.replaceLocked(next); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// Revise appends a new revision. because is required: a revision nobody can
// explain is worse than no revision, because it looks like the plan always
// said that.
func (s *Store) Revise(id string, steps []Step, by, because string) (Plan, error) {
	if because == "" {
		return Plan{}, fmt.Errorf("a plan revision must say what triggered it")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	revisions := s.data.Plans[id]
	if len(revisions) == 0 {
		return Plan{}, fmt.Errorf("plan %s not found", id)
	}
	latest := revisions[len(revisions)-1]
	next := latest
	next.Rev = latest.Rev + 1
	next.Steps = cloneSteps(steps)
	next.By = by
	next.Because = because
	next.CreatedAt = s.now()
	if err := Validate(next); err != nil {
		return Plan{}, err
	}
	staged := s.clone()
	staged.Plans[id] = append(staged.Plans[id], next)
	if err := s.replaceLocked(staged); err != nil {
		return Plan{}, err
	}
	return next, nil
}

// Advance records step progress inside the current revision. Progress is not
// a revision: only a change of intent is.
func (s *Store) Advance(id string, step Step) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revisions := s.data.Plans[id]
	if len(revisions) == 0 {
		return Plan{}, fmt.Errorf("plan %s not found", id)
	}
	staged := s.clone()
	current := &staged.Plans[id][len(staged.Plans[id])-1]
	found := false
	for i := range current.Steps {
		if current.Steps[i].ID == step.ID {
			current.Steps[i] = step
			found = true
			break
		}
	}
	if !found {
		return Plan{}, fmt.Errorf("plan %s has no step %q", id, step.ID)
	}
	if err := s.replaceLocked(staged); err != nil {
		return Plan{}, err
	}
	return *current, nil
}

// RecordStep is the executor's write-back: progress inside the current
// revision, not a new one. Only a change of intent makes a revision.
func (s *Store) RecordStep(planID string, step Step) error {
	_, err := s.Advance(planID, step)
	return err
}

// Latest returns the current revision.
func (s *Store) Latest(id string) (Plan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revisions := s.data.Plans[id]
	if len(revisions) == 0 {
		return Plan{}, false
	}
	return clonePlan(revisions[len(revisions)-1]), true
}

// Revisions returns the full history, oldest first.
func (s *Store) Revisions(id string) []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Plan, 0, len(s.data.Plans[id]))
	for _, p := range s.data.Plans[id] {
		out = append(out, clonePlan(p))
	}
	return out
}

// ForTask finds the plan driving a task.
func (s *Store) ForTask(taskID string) (Plan, bool) {
	s.mu.Lock()
	id := s.data.ByTask[taskID]
	s.mu.Unlock()
	if id == "" {
		return Plan{}, false
	}
	return s.Latest(id)
}

// List returns the current revision of every plan, newest first.
func (s *Store) List() []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Plan, 0, len(s.data.Plans))
	for _, revisions := range s.data.Plans {
		if len(revisions) > 0 {
			out = append(out, clonePlan(revisions[len(revisions)-1]))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func clonePlan(p Plan) Plan {
	if p.Execution != nil {
		token := *p.Execution
		p.Execution = &token
	}
	p.Steps = cloneSteps(p.Steps)
	return p
}

func cloneSteps(steps []Step) []Step {
	out := make([]Step, len(steps))
	for i, s := range steps {
		copied := s
		copied.Needs = append([]string(nil), s.Needs...)
		copied.Merge = append([]string(nil), s.Merge...)
		copied.Requires = append([]string(nil), s.Requires...)
		copied.Tried = append([]string(nil), s.Tried...)
		if s.Verify != nil {
			verify := *s.Verify
			copied.Verify = &verify
		}
		if s.Result != nil {
			result := *s.Result
			result.Refs = append([]Ref(nil), s.Result.Refs...)
			result.Findings = append([]Finding(nil), s.Result.Findings...)
			copied.Result = &result
		}
		out[i] = copied
	}
	return out
}

func (s *Store) clone() data {
	next := data{
		NextID: s.data.NextID,
		Plans:  make(map[string][]Plan, len(s.data.Plans)),
		ByTask: make(map[string]string, len(s.data.ByTask)),
	}
	for id, revisions := range s.data.Plans {
		copied := make([]Plan, len(revisions))
		for i, p := range revisions {
			copied[i] = clonePlan(p)
		}
		next.Plans[id] = copied
	}
	for task, id := range s.data.ByTask {
		next.ByTask[task] = id
	}
	return next
}

func (s *Store) replaceLocked(next data) error {
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plans: %w", err)
	}
	if err := s.doc.Save(raw); err != nil {
		return fmt.Errorf("save plans: %w", err)
	}
	s.data = next
	return nil
}
