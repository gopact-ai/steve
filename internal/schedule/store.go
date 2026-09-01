package schedule

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

// Job is one standing instruction: what to run, where to say it, and when.
// It carries its own anchor because a schedule outlives the conversation's
// scrollback — when it fires days later, that anchor is the only thing that
// still says which chat and which topic it belongs to.
type Job struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	ChatID         string    `json:"chat_id,omitempty"`
	ChatType       string    `json:"chat_type,omitempty"`
	AnchorMessage  string    `json:"anchor_message"`
	Requester      string    `json:"requester,omitempty"`
	Member         string    `json:"member,omitempty"`
	Prompt         string    `json:"prompt"`
	Spec           Spec      `json:"spec"`
	NextAt         time.Time `json:"next_at"`
	LastAt         time.Time `json:"last_at,omitzero"`
	Runs           int       `json:"runs,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// MaxPerConversation bounds how many standing instructions one chat can hold.
// A schedule is unattended work: the ceiling is what keeps a typo from turning
// into a chat that talks to itself forever.
const MaxPerConversation = 8

// Store persists jobs with the same durable-replace discipline as the task
// store: one writer, a temp file, a rename, a directory sync.
type Store struct {
	path string
	mu   sync.Mutex
	data data
	now  func() time.Time
}

type data struct {
	NextID int             `json:"next_id"`
	Jobs   map[string]*Job `json:"jobs"`
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, data: data{NextID: 1, Jobs: map[string]*Job{}}, now: time.Now}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(raw) == 0) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read schedules: %w", err)
	}
	var loaded data
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("decode schedules: %w", err)
	}
	if loaded.Jobs == nil {
		loaded.Jobs = map[string]*Job{}
	}
	if loaded.NextID < 1 {
		loaded.NextID = 1
	}
	s.data = loaded
	return s, nil
}

// Create stores the job and computes its first firing. It refuses a spec that
// can never fire again, so a schedule the user cannot see working is rejected
// at the moment they can still fix it.
func (s *Store) Create(job Job) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	next := job.Spec.Next(now)
	if next.IsZero() {
		return Job{}, fmt.Errorf("schedule would never fire")
	}
	held := 0
	for _, stored := range s.data.Jobs {
		if stored.ConversationID == job.ConversationID {
			held++
		}
	}
	if held >= MaxPerConversation {
		return Job{}, fmt.Errorf("this conversation already holds %d schedules", held)
	}
	job.ID = strconv.Itoa(s.data.NextID)
	job.CreatedAt = now
	job.NextAt = next
	replacement := s.clone()
	replacement.NextID = s.data.NextID + 1
	replacement.Jobs[job.ID] = &job
	if err := s.replaceLocked(replacement); err != nil {
		return Job{}, err
	}
	return job, nil
}

// List returns a conversation's jobs, soonest first. An empty conversation
// lists every job, which is what the firing loop wants.
func (s *Store) List(conversationID string) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.data.Jobs))
	for _, stored := range s.data.Jobs {
		if conversationID != "" && stored.ConversationID != conversationID {
			continue
		}
		out = append(out, *stored)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NextAt.Equal(out[j].NextAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].NextAt.Before(out[j].NextAt)
	})
	return out
}

func (s *Store) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Jobs[id]
	if !ok {
		return Job{}, false
	}
	return *stored, true
}

// Delete removes a job. Cancelling a schedule is a deletion rather than a
// tombstone: unlike a task it has no history worth keeping — the runs it
// produced are already tasks of their own.
func (s *Store) Delete(id string) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Jobs[id]
	if !ok {
		return Job{}, false, nil
	}
	removed := *stored
	replacement := s.clone()
	delete(replacement.Jobs, id)
	if err := s.replaceLocked(replacement); err != nil {
		return Job{}, false, err
	}
	return removed, true, nil
}

// MaxLateness is how far past its moment a firing is still worth doing. A
// gateway that was off overnight should not open the morning by running every
// job it slept through: the moment those instructions were about has passed,
// and the schedule's next turn comes round soon enough.
const MaxLateness = time.Hour

// Due claims every job whose moment has come and advances it in the same
// write. Claiming and advancing together is what stops a slow firing from
// being fired again by the next tick.
func (s *Store) Due(now time.Time) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	replacement := s.clone()
	var claimed []Job
	changed := false
	for id, stored := range replacement.Jobs {
		if stored.NextAt.IsZero() || stored.NextAt.After(now) {
			continue
		}
		changed = true
		if now.Sub(stored.NextAt) <= MaxLateness {
			stored.LastAt = now
			stored.Runs++
			claimed = append(claimed, *stored)
		}
		next := stored.Spec.Next(now)
		if !stored.Spec.Recurring() || next.IsZero() {
			// A one-shot has said everything it had to say.
			delete(replacement.Jobs, id)
			continue
		}
		stored.NextAt = next
	}
	if changed && len(claimed) == 0 {
		// Nothing to run, but the skipped jobs still moved on.
		if err := s.replaceLocked(replacement); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	if err := s.replaceLocked(replacement); err != nil {
		return nil, err
	}
	sort.Slice(claimed, func(i, j int) bool { return claimed[i].ID < claimed[j].ID })
	return claimed, nil
}

func (s *Store) clone() data {
	next := data{NextID: s.data.NextID, Jobs: make(map[string]*Job, len(s.data.Jobs))}
	for id, stored := range s.data.Jobs {
		copied := *stored
		next.Jobs[id] = &copied
	}
	return next
}

func (s *Store) replaceLocked(next data) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create schedule directory: %w", err)
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode schedules: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".schedules-*")
	if err != nil {
		return fmt.Errorf("create schedule file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure schedule file: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("write schedules: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync schedules: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close schedules: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replace schedules: %w", err)
	}
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("sync schedule directory: %w", err)
	}
	s.data = next
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
