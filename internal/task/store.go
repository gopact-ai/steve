package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Store persists tasks with the same durable-replace discipline as the session
// state file. It stays JSON on purpose: the gateway is the only writer, and the
// data that would outgrow a single file — transcripts and tree snapshots — is
// meant to land in a sharded archive on disk, not in here.
type Store struct {
	path string
	mu   sync.Mutex
	data data
	now  func() time.Time
	// Default budgets for new tasks; configurable so long-running work is
	// a deployment decision, not a code change.
	maxTurns   int
	maxElapsed time.Duration
}

type data struct {
	NextID int              `json:"next_id"`
	Tasks  map[string]*Task `json:"tasks"`
}

func Open(path string) (*Store, error) {
	s := &Store{
		path: path, data: data{NextID: 1, Tasks: map[string]*Task{}}, now: time.Now,
		maxTurns: DefaultMaxTurns, maxElapsed: DefaultMaxElapsed,
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	if len(raw) == 0 {
		return s, nil
	}
	var loaded data
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	if loaded.Tasks == nil {
		loaded.Tasks = map[string]*Task{}
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
	return t, nil
}

// Active returns the newest unfinished task this member holds on the channel.
// A task is scoped to (channel, member) because it is attempted through that
// member's session: resetting the session is what ends the task.
func (s *Store) Active(channel, member string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var newest *Task
	for _, stored := range s.data.Tasks {
		if stored.Channel != channel || stored.Member != member || stored.State.Terminal() {
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
			return out[i].ID > out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
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
func (s *Store) SetAnchor(id, chatID, messageID, chatType string) error {
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
	stored.UpdatedAt = s.now()
	if err := s.replaceLocked(next); err != nil {
		return err
	}
	return nil
}

// Interrupted lists non-terminal tasks whose latest attempt never ended —
// the gateway died mid-turn. Newest first.
func (s *Store) Interrupted() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, 2)
	for _, stored := range s.data.Tasks {
		if stored.State.Terminal() {
			continue
		}
		if n := len(stored.Attempts); n > 0 && stored.Attempts[n-1].Open() {
			out = append(out, *stored.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Begin opens an attempt and moves the task to running. It refuses when the
// budget is spent so a runaway loop stops at the store rather than relying on
// every caller to remember to check.
func (s *Store) Begin(id, member, node, session string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	stored, ok := next.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if limit, spent := stored.Budget.Exhausted(); spent {
		return Task{}, fmt.Errorf("task %s budget exhausted: %s", id, limit)
	}
	if !stored.State.CanMoveTo(StateRunning) {
		return Task{}, fmt.Errorf("task %s cannot run from %s", id, stored.State)
	}
	now := s.now()
	stored.State = StateRunning
	stored.Member = member
	stored.Node = node
	stored.Budget.Turns++
	stored.UpdatedAt = now
	stored.Attempts = append(stored.Attempts, Attempt{
		Member: member, Node: node, Session: session, StartedAt: now,
	})
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *stored.clone(), nil
}

// Finish closes the open attempt and folds its cost into the budget. The task
// state is left to the caller: a finished turn is not a finished task.
func (s *Store) Finish(id string, outcome Outcome, tokens Tokens, toolCalls int) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	stored, ok := next.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if len(stored.Attempts) == 0 {
		return Task{}, fmt.Errorf("task %s has no attempt to finish", id)
	}
	attempt := &stored.Attempts[len(stored.Attempts)-1]
	if !attempt.Open() {
		return Task{}, fmt.Errorf("task %s attempt already finished", id)
	}
	now := s.now()
	attempt.EndedAt = now
	attempt.Outcome = outcome
	attempt.Tokens = tokens
	stored.Budget.Elapsed += now.Sub(attempt.StartedAt)
	stored.Budget.ToolCalls += toolCalls
	stored.Budget.Tokens = stored.Budget.Tokens.Add(tokens)
	stored.UpdatedAt = now
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
	stored.State = to
	stored.UpdatedAt = s.now()
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *stored.clone(), nil
}

func (t *Task) clone() *Task {
	copied := *t
	copied.Attempts = append([]Attempt(nil), t.Attempts...)
	return &copied
}

func (s *Store) clone() data {
	next := data{NextID: s.data.NextID, Tasks: make(map[string]*Task, len(s.data.Tasks))}
	for id, stored := range s.data.Tasks {
		next.Tasks[id] = stored.clone()
	}
	return next
}

func (s *Store) replaceLocked(next data) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create task directory: %w", err)
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tasks: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".tasks-*")
	if err != nil {
		return fmt.Errorf("create task file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure task file: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("write tasks: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync tasks: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close tasks: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replace tasks: %w", err)
	}
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("sync task directory: %w", err)
	}
	s.data = next
	return nil
}

// syncDir flushes a directory entry after a rename so the replacement survives
// a crash (rename alone is not guaranteed durable on all filesystems).
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
