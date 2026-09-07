package task

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// Store persists tasks with the same durable-replace discipline as the session
// state file. It stays JSON on purpose: the gateway is the only writer, and the
// data that would outgrow a single file — transcripts and tree snapshots — is
// meant to land in a sharded archive on disk, not in here.
type Store struct {
	book *ledger.Ledger
	doc  ledger.Doc
	mu   sync.Mutex
	data data
	now  func() time.Time
	// Default budgets for new tasks; configurable so long-running work is
	// a deployment decision, not a code change.
	maxTurns   int
	maxElapsed time.Duration
	// observe is told each task id a write changed, after the write
	// landed; it runs off the store's lock.
	observe func(id string)
}

// SetObserver installs where task changes are announced; nil discards.
func (s *Store) SetObserver(observe func(id string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe = observe
}

type data struct {
	NextID int              `json:"next_id"`
	Tasks  map[string]*Task `json:"tasks"`
	Meta   map[string]Meta  `json:"meta,omitempty"`
}

// Open keeps the store in one JSON file. It is what tests use and what a
// pre-ledger deployment wrote; the gateway itself opens the ledger.
func Open(path string) (*Store, error) {
	return openWith(&ledger.FileDocument{Path: path})
}

// OpenLedger keeps the store in the ledger. legacy names the JSON file an
// earlier deployment used; it is imported once and retired.
func OpenLedger(l *ledger.Ledger, legacy string) (*Store, error) {
	doc := l.Document("tasks")
	if _, err := doc.Import(legacy); err != nil {
		return nil, err
	}
	s, err := openWith(doc)
	if err == nil {
		s.book = l
	}
	return s, err
}

func openWith(doc ledger.Doc) (*Store, error) {
	s := &Store{
		doc: doc, data: data{NextID: 1, Tasks: map[string]*Task{}, Meta: map[string]Meta{}}, now: time.Now,
		maxTurns: DefaultMaxTurns, maxElapsed: DefaultMaxElapsed,
	}
	raw, ok, err := doc.Load()
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	if !ok || len(raw) == 0 {
		return s, nil
	}
	var loaded data
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	if loaded.Tasks == nil {
		loaded.Tasks = map[string]*Task{}
	}
	if loaded.Meta == nil {
		loaded.Meta = map[string]Meta{}
	}
	if loaded.NextID < 1 {
		loaded.NextID = 1
	}
	s.data = loaded
	return s, nil
}

// Create assigns the id and returns the stored copy. Goal is trimmed to keep
// listings readable; the full prompt lives in the archive, not here.
func (s *Store) Create(t Task) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	t.ID = strconv.Itoa(s.data.NextID)
	t.State = StateDraft
	t.ExecutionEpoch = 1
	if t.PreparedPlan != nil {
		prepared := *t.PreparedPlan
		prepared.Execution = ExecutionToken{TaskID: t.ID, Epoch: t.ExecutionEpoch}
		prepared.Snapshot = append([]byte(nil), prepared.Snapshot...)
		t.PreparedPlan = &prepared
	}
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Budget.MaxTurns == 0 {
		t.Budget.MaxTurns = s.maxTurns
	}
	if t.Budget.MaxElapsed == 0 {
		t.Budget.MaxElapsed = s.maxElapsed
	}
	next := s.clone()
	next.NextID = s.data.NextID + 1
	next.Tasks[t.ID] = &t
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *t.clone(), nil
}

// Active returns the newest unfinished task this member holds on the channel
// for the given origin. A task is scoped to (channel, member) because it is
// attempted through that member's session: resetting the session is what ends
// the task. Origin partitions it further — unattended work and what a person
// typed are separate lineages, so a schedule's nightly run never charges its
// turns to the task the user is in the middle of, or the other way round.
//
// A paused task is deliberately not active: setting work aside has to leave
// room for the next thing the user asks for.
func (s *Store) Active(channel, member, origin string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newestLocked(func(stored *Task) bool {
		return stored.Channel == channel && stored.Member == member &&
			stored.Origin == origin && stored.State.Holds()
	})
}

// Running returns the task whose attempt is open right now — the one a turn
// in flight belongs to, whoever or whatever started it.
func (s *Store) Running(channel, member string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newestLocked(func(stored *Task) bool {
		if stored.Channel != channel || stored.Member != member || !stored.State.Holds() {
			return false
		}
		return stored.HasOpenExecution()
	})
}

// Holding lists every unfinished task this member holds on the channel,
// across origins. A session reset ends all of them: they were all being
// attempted through the session that just went away.
func (s *Store) Holding(channel, member string) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	for _, stored := range s.data.Tasks {
		if stored.Channel == channel && stored.Member == member && stored.State.Holds() {
			out = append(out, *stored.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func (s *Store) newestLocked(match func(*Task) bool) (Task, bool) {
	var newest *Task
	for _, stored := range s.data.Tasks {
		if !match(stored) {
			continue
		}
		if newest == nil || stored.UpdatedAt.After(newest.UpdatedAt) {
			newest = stored
		}
	}
	if newest == nil {
		return Task{}, false
	}
	return *newest.clone(), true
}

func (s *Store) Get(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[id]
	if !ok {
		return Task{}, false
	}
	return *stored.clone(), true
}

// List returns tasks newest first. An empty channel lists every channel.
func (s *Store) List(channel string) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, len(s.data.Tasks))
	for _, stored := range s.data.Tasks {
		if channel != "" && stored.Channel != channel {
			continue
		}
		out = append(out, *stored.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return lessID(out[j].ID, out[i].ID)
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// CloseIdle ends chat tasks nobody has touched for longer than age: a
// thread that stopped being spoken to has finished, and a task that
// keeps counting as running for it misleads every list. Only tasks a
// person opened by talking (no origin) are closed, only when nothing is
// live on them, and only past the age. The closed tasks are returned.
func (s *Store) CloseIdle(age time.Duration, live func(id string) bool) []Task {
	cutoff := s.now().Add(-age)
	var closed []Task
	for _, t := range s.List("") {
		if t.State != StateRunning || t.Origin != "" || !t.UpdatedAt.Before(cutoff) {
			continue
		}
		if live != nil && live(t.ID) {
			continue
		}
		done, err := s.Advance(t.ID, StateDone)
		if err != nil {
			continue
		}
		closed = append(closed, done)
	}
	return closed
}

// SetBudget raises the default budgets new tasks are created with.
// Non-positive values keep the current default.
func (s *Store) SetBudget(maxTurns int, maxElapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxTurns > 0 {
		s.maxTurns = maxTurns
	}
	if maxElapsed > 0 {
		s.maxElapsed = maxElapsed
	}
}

// SetAnchor records where the task's latest turn is anchored in the chat,
// so a restarted gateway can deliver into the right conversation.
func (s *Store) SetAnchor(id, chatID, messageID, chatType, cardID string) error {
	if messageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	stored, ok := next.Tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	stored.ChatID = chatID
	stored.AnchorMessage = messageID
	stored.ChatType = chatType
	// A new turn starts a clean leftover slate.
	stored.OpenCard = cardID
	stored.Interim = nil
	stored.UpdatedAt = s.now()
	if err := s.replaceLocked(next); err != nil {
		return err
	}
	return nil
}

// maxInterim bounds the leftover journal; older entries drop first.
const maxInterim = 16

// AddInterim records one message the member's agent sent during the current
// turn, so a crash between send and finish leaves nothing untraceable. It
// journals against the task whose attempt is open: the message belongs to the
// turn that is running, not to whichever lineage was touched last.
func (s *Store) AddInterim(channel, member, messageID string) error {
	if messageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	newest, ok := s.newestLocked(func(stored *Task) bool {
		if stored.Channel != channel || stored.Member != member || !stored.State.Holds() {
			return false
		}
		return stored.HasOpenExecution()
	})
	if !ok {
		return fmt.Errorf("no running task for %s/%s", channel, member)
	}
	return s.addInterimLocked(newest.ID, messageID)
}

// AddInterimForTask records a sent message against the task that issued it.
// A delayed receipt still belongs there after another task starts running.
func (s *Store) AddInterimForTask(taskID, messageID string) error {
	if messageID == "" {
		return fmt.Errorf("interim message id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addInterimLocked(taskID, messageID)
}

func (s *Store) addInterimLocked(taskID, messageID string) error {
	if _, ok := s.data.Tasks[taskID]; !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	next := s.clone()
	stored := next.Tasks[taskID]
	stored.Interim = append(stored.Interim, messageID)
	if len(stored.Interim) > maxInterim {
		stored.Interim = stored.Interim[len(stored.Interim)-maxInterim:]
	}
	stored.UpdatedAt = s.now()
	return s.replaceLocked(next)
}

// Interrupted lists non-terminal tasks with an open current execution —
// the gateway died mid-turn. Newest first.
func (s *Store) Interrupted() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, 2)
	for _, stored := range s.data.Tasks {
		if stored.State.Terminal() {
			continue
		}
		if stored.HasOpenExecution() {
			out = append(out, *stored.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Begin opens an attempt and charges one turn to this task and every
// ancestor in the same durable write. Concurrent siblings share the same
// ancestor budget; a task may have only one open attempt of its own.
func (s *Store) Begin(id, member, node, session string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	stored, ok := next.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if row := stored.primaryAttempt(); row != nil && row.Open() {
		return Task{}, fmt.Errorf("task %s already has an open attempt", id)
	}
	if !stored.State.Holds() || !stored.State.CanMoveTo(StateRunning) {
		return Task{}, fmt.Errorf("task %s cannot run from %s", id, stored.State)
	}
	now := s.now()
	if err := reserveTurn(next.Tasks, id, now); err != nil {
		return Task{}, err
	}
	stored.State = StateRunning
	stored.Member = member
	stored.Node = node
	stored.Attempts = append(stored.Attempts, Attempt{
		Member: member, Node: node, Session: session, StartedAt: now, ExecutionEpoch: stored.ExecutionEpoch,
	})
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *stored.clone(), nil
}

// ReserveTurn charges one independently tracked execution, such as a plan
// step, without opening a synthetic conversation attempt. Admission and the
// charge to every ancestor share one durable update.
func (s *Store) ReserveTurn(id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	stored, ok := next.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if !stored.State.Holds() || !stored.State.CanMoveTo(StateRunning) {
		return Task{}, fmt.Errorf("task %s cannot run from %s", id, stored.State)
	}
	if err := reserveTurn(next.Tasks, id, s.now()); err != nil {
		return Task{}, err
	}
	stored.State = StateRunning
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *stored.clone(), nil
}

func reserveTurn(tasks map[string]*Task, id string, now time.Time) error {
	lineage, err := taskLineage(tasks, id)
	if err != nil {
		return err
	}
	for _, member := range lineage {
		if member.State == StatePaused || member.State == StateCancelled {
			return fmt.Errorf("%w: ancestor %s", ErrExecutionStopped, member.ID)
		}
		if limit, spent := member.Budget.Exhausted(); spent {
			return fmt.Errorf("task %s budget exhausted: %s", member.ID, limit)
		}
	}
	for _, member := range lineage {
		member.Budget.Turns++
		member.UpdatedAt = now
	}
	return nil
}

// Finish closes the open attempt and atomically adds only that execution's
// elapsed time, tool calls and tokens to this task and every ancestor.
// The task state is left to the caller: a finished turn is not a finished task.
func (s *Store) Finish(id string, outcome Outcome, tokens Tokens, toolCalls int) (Task, error) {
	return s.FinishAs(id, outcome, tokens, toolCalls, "")
}

// FinishAs is Finish with the model the attempt ran on.
func (s *Store) FinishAs(id string, outcome Outcome, tokens Tokens, toolCalls int, model string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	stored, ok := next.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	attempt := stored.primaryAttempt()
	if attempt == nil {
		return Task{}, fmt.Errorf("task %s has no attempt to finish", id)
	}
	if !attempt.Open() {
		return Task{}, fmt.Errorf("task %s attempt already finished", id)
	}
	lineage, err := taskLineage(next.Tasks, id)
	if err != nil {
		return Task{}, err
	}
	now := s.now()
	attempt.EndedAt = now
	attempt.Outcome = outcome
	attempt.Tokens = tokens
	attempt.Model = model
	for _, member := range lineage {
		member.Budget.Elapsed += now.Sub(attempt.StartedAt)
		member.Budget.ToolCalls += toolCalls
		member.Budget.Tokens = member.Budget.Tokens.Add(tokens)
		member.UpdatedAt = now
	}
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *stored.clone(), nil
}

func (s *Store) Advance(id string, to State) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	stored, ok := next.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if !stored.State.CanMoveTo(to) {
		return Task{}, fmt.Errorf("task %s cannot move %s -> %s", id, stored.State, to)
	}
	if stored.State == StatePaused && to == StateRunning {
		stored.ExecutionEpoch++
	}
	stored.State = to
	stored.UpdatedAt = s.now()
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *stored.clone(), nil
}

// lessID orders ids numerically. They are decimal counters, so comparing
// them as text puts #10 before #2 — which a listing of more than nine tasks
// shows the user directly.
func lessID(a, b string) bool {
	na, aerr := strconv.Atoi(a)
	nb, berr := strconv.Atoi(b)
	if aerr != nil || berr != nil {
		return a < b
	}
	return na < nb
}

func (t *Task) clone() *Task {
	copied := *t
	if t.PreparedPlan != nil {
		prepared := *t.PreparedPlan
		prepared.Snapshot = append([]byte(nil), prepared.Snapshot...)
		copied.PreparedPlan = &prepared
	}
	copied.Attempts = append([]Attempt(nil), t.Attempts...)
	for i := range copied.Attempts {
		if copied.Attempts[i].UsageKnown != nil {
			known := *copied.Attempts[i].UsageKnown
			copied.Attempts[i].UsageKnown = &known
		}
	}
	copied.Interim = append([]string(nil), t.Interim...)
	if t.RecoveryWorkspace != nil {
		workspace := *t.RecoveryWorkspace
		copied.RecoveryWorkspace = &workspace
	}
	if t.Result != nil {
		r := *t.Result
		r.Refs = append([]string(nil), t.Result.Refs...)
		copied.Result = &r
	}
	if t.Delivery != nil {
		d := *t.Delivery
		copied.Delivery = &d
	}
	return &copied
}

func (s *Store) clone() data {
	next := data{NextID: s.data.NextID, Tasks: make(map[string]*Task, len(s.data.Tasks)), Meta: make(map[string]Meta, len(s.data.Meta))}
	for id, stored := range s.data.Tasks {
		next.Tasks[id] = stored.clone()
	}
	for id, meta := range s.data.Meta {
		next.Meta[id] = meta.clone()
	}
	return next
}

func (s *Store) replaceLocked(next data) error {
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tasks: %w", err)
	}
	if err := s.doc.Save(raw); err != nil {
		return fmt.Errorf("save tasks: %w", err)
	}
	s.installLocked(next)
	return nil
}

func (s *Store) installLocked(next data) {
	if s.observe != nil {
		var changed []string
		for id, t := range next.Tasks {
			if prev, ok := s.data.Tasks[id]; !ok || prev.State != t.State || !prev.UpdatedAt.Equal(t.UpdatedAt) || !s.data.Meta[id].equal(next.Meta[id]) {
				changed = append(changed, id)
			}
		}
		for id := range s.data.Tasks {
			if _, ok := next.Tasks[id]; !ok {
				changed = append(changed, id)
			}
		}
		if len(changed) > 0 {
			observe := s.observe
			go func() {
				for _, id := range changed {
					observe(id)
				}
			}()
		}
	}
	s.data = next
}
