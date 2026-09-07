package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// Job is one standing instruction: what to run, where to say it, and when.
// It carries its own anchor because a schedule outlives the conversation's
// scrollback — when it fires days later, that anchor is the only thing that
// still says which chat and which topic it belongs to.
type Job struct {
	ID             string    `json:"id"`
	Channel        string    `json:"channel"`
	ProjectID      string    `json:"project_id,omitempty"`
	State          string    `json:"-"`
	Error          string    `json:"-"`
	PendingKey     string    `json:"-"`
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
	doc    ledger.Doc
	mu     sync.Mutex
	data   data
	now    func() time.Time
	active map[string]bool
}

type data struct {
	NextID  int                `json:"next_id"`
	Jobs    map[string]*Job    `json:"jobs"`
	Firings map[string]*Firing `json:"firings,omitempty"`
}

// Open keeps the store in one JSON file; the gateway opens the ledger.
func Open(path string) (*Store, error) {
	return openWith(&ledger.FileDocument{Path: path})
}

// OpenLedger keeps the store in the ledger, importing a legacy file once.
func OpenLedger(l *ledger.Ledger, legacy string) (*Store, error) {
	doc := l.Document("schedules")
	if _, err := doc.Import(legacy); err != nil {
		return nil, err
	}
	return openWith(doc)
}

func openWith(doc ledger.Doc) (*Store, error) {
	s := &Store{doc: doc, data: data{NextID: 1, Jobs: map[string]*Job{}, Firings: map[string]*Firing{}}, now: time.Now, active: map[string]bool{}}
	raw, ok, err := doc.Load()
	if err != nil {
		return nil, fmt.Errorf("read schedules: %w", err)
	}
	if !ok || len(raw) == 0 {
		return s, nil
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
	if loaded.Firings == nil {
		loaded.Firings = map[string]*Firing{}
	}
	s.data = loaded
	if err := s.recoverFirings(); err != nil {
		return nil, err
	}
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
		out = append(out, s.describeLocked(*stored))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NextAt.Equal(out[j].NextAt) {
			return lessID(out[i].ID, out[j].ID)
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
	return s.describeLocked(*stored), true
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
	for _, f := range replacement.Firings {
		if f.ID != id {
			continue
		}
		if f.State == FiringDispatching || f.State == FiringUnknown {
			return Job{}, false, fmt.Errorf("schedule %s has an unresolved firing; confirm it or authorize retry first", id)
		}
		if f.State == FiringPending {
			f.State = FiringCancelled
		}
	}
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

// lessID orders ids the way the person reading them does. They are decimal
// counters, so comparing them as text puts #10 before #2 the moment a
// conversation gets past its ninth schedule.
func lessID(a, b string) bool {
	na, aerr := strconv.Atoi(a)
	nb, berr := strconv.Atoi(b)
	if aerr != nil || berr != nil {
		return a < b
	}
	return na < nb
}

func (s *Store) clone() data {
	next := data{NextID: s.data.NextID, Jobs: make(map[string]*Job, len(s.data.Jobs)), Firings: make(map[string]*Firing, len(s.data.Firings))}
	for id, stored := range s.data.Jobs {
		copied := *stored
		next.Jobs[id] = &copied
	}
	for key, firing := range s.data.Firings {
		copied := *firing
		next.Firings[key] = &copied
	}
	return next
}

func (s *Store) replaceLocked(next data) error {
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode schedules: %w", err)
	}
	if err := s.doc.Save(raw); err != nil {
		return fmt.Errorf("save schedules: %w", err)
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
