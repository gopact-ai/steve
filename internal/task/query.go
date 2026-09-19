package task

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

const RecentClosedLimit = 20
const MaxQueryLimit = 100

var (
	ErrInvalidQuery = errors.New("invalid task query or cursor")
	ErrStaleCursor  = errors.New("task query changed; refresh from the first page")
	ErrTaskNotFound = errors.New("task not found")
)

// Header contains no accounting rows. Summary counts own accounting rather
// than Budget, whose costs include descendants. Meta is owned by the task store.
type Header struct {
	Task
	Meta    Meta        `json:"meta"`
	Summary ReadSummary `json:"summary"`
}
type ReadSummary struct {
	Attempts       int    `json:"attempts"`
	OpenExecutions int    `json:"open_executions"`
	Children       int    `json:"children"`
	Tokens         Tokens `json:"tokens"`
	Seconds        int64  `json:"seconds"`
	Model          string `json:"model,omitempty"`
	CanComplete    bool   `json:"can_complete"`
	PlanInTree     bool   `json:"plan_in_tree"`
}

// Scope selects a prebuilt ordered index, not a post-filter of a global page.
// Empty kind selects every task; children selects direct children only.
type Scope struct {
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
}
type Query struct {
	Scope    Scope  `json:"scope"`
	Status   string `json:"status,omitempty"`   // all, live, closed
	Archived string `json:"archived,omitempty"` // all, hide, only
	Cursor   string `json:"cursor,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}
type Page struct {
	Items      []Header `json:"items"`
	Total      int      `json:"total"`
	NextCursor string   `json:"next_cursor,omitempty"`
}
type Counts struct {
	Total          int `json:"total"`
	Live           int `json:"live"`
	Closed         int `json:"closed"`
	Roots          int `json:"roots"`
	CompletedRoots int `json:"completed_roots"`
	CancelledRoots int `json:"cancelled_roots"`
	PausedRoots    int `json:"paused_roots"`
}
type Coverage struct {
	Counts
	Included      int      `json:"included"`
	RecentLimit   int      `json:"recent_limit"`
	RecentClosed  int      `json:"recent_closed"`
	HasMoreClosed bool     `json:"has_more_closed"`
	Missing       []string `json:"missing,omitempty"`
}
type Workset struct {
	Items    []Header `json:"items"`
	Coverage Coverage `json:"coverage"`
}
type AccountingRow struct {
	Index int `json:"index"`
	Attempt
}
type AccountingPage struct {
	Items      []AccountingRow `json:"items"`
	Total      int             `json:"total"`
	NextCursor string          `json:"next_cursor,omitempty"`
}
type queryKey struct {
	Scope
	Status, Archived string
}
type readIndex struct {
	nonce       string
	revision    uint64
	versions    map[queryKey]uint64
	ordered     map[queryKey][]string
	summaries   map[string]ReadSummary
	counts      map[Scope]Counts
	planTasks   map[string]bool
	trees       map[string]treeSummary
	models      map[string][]int
	primary     map[string][]int
	primaryRows map[string][]int
}

func taskBefore(a, b *Task) bool {
	if a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.ID > b.ID
	}
	return a.UpdatedAt.After(b.UpdatedAt)
}
func actionable(t Task, summary ReadSummary) bool {
	if summary.OpenExecutions > 0 || (!t.Settled() && !t.State.Terminal()) {
		return true
	}
	if t.Delivery != nil {
		switch t.Delivery.State {
		case DeliveryPending, DeliveryQueued, DeliveryUncertain:
			return true
		}
	}
	return t.Delegated() && t.Parent != "" && t.Finished() && t.Result != nil && t.Delivery == nil
}
func (s *Store) headerLocked(id string) (Header, bool) {
	t, ok := s.data.Tasks[id]
	if !ok {
		return Header{}, false
	}
	head := headOf(t).Task
	return Header{Task: *head.clone(), Meta: s.data.Meta[id].clone(), Summary: s.readIndex.summaries[id]}, true
}
func (s *Store) Header(id string) (Header, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headerLocked(id)
}
func (s *Store) Counts(scope Scope) Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readIndex.counts[scope]
}

// Workset returns every actionable task, the latest 20 closed tasks, references
// from other live owners, and all their ancestors. Historic descendants are not
// implied by membership; Summary.Children and Query(children) expose coverage.
func (s *Store) Workset(references []string) Workset {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.readIndex.ordered[queryKey{Status: "live"}]
	closed := s.readIndex.ordered[queryKey{Status: "closed"}]
	recent := closed[:min(RecentClosedLimit, len(closed))]
	selected := map[string]bool{}
	missing := map[string]bool{}
	add := func(id string) {
		for id != "" && !selected[id] && !missing[id] {
			t, ok := s.data.Tasks[id]
			if !ok {
				missing[id] = true
				break
			}
			selected[id] = true
			id = t.Parent
		}
	}
	for _, id := range live {
		add(id)
	}
	for _, id := range recent {
		add(id)
	}
	for _, id := range references {
		add(id)
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return taskBefore(s.data.Tasks[ids[i]], s.data.Tasks[ids[j]]) })
	out := Workset{Items: make([]Header, 0, len(ids)), Coverage: Coverage{Counts: s.readIndex.counts[Scope{}], Included: len(ids), RecentLimit: RecentClosedLimit, RecentClosed: len(recent)}}
	includedClosed := 0
	for _, id := range ids {
		h, _ := s.headerLocked(id)
		out.Items = append(out.Items, h)
		if !actionable(h.Task, h.Summary) {
			includedClosed++
		}
	}
	out.Coverage.HasMoreClosed = includedClosed < out.Coverage.Closed
	for id := range missing {
		out.Coverage.Missing = append(out.Coverage.Missing, id)
	}
	sort.Strings(out.Coverage.Missing)
	return out
}

// The opaque cursor is bound to this owner instance, selected query revision and
// exact scope/filter. Mutations explicitly invalidate pages instead of silently
// skipping or repeating records whose sort keys changed.
type queryCursor struct {
	Version  int    `json:"v"`
	Owner    string `json:"owner"`
	Revision uint64 `json:"revision"`
	Scope    Scope  `json:"scope"`
	Status   string `json:"status,omitempty"`
	Archived string `json:"archived,omitempty"`
	Offset   int    `json:"offset"`
}

func pageLimit(limit int) (int, error) {
	if limit < 0 || limit > MaxQueryLimit {
		return 0, ErrInvalidQuery
	}
	if limit == 0 {
		limit = 50
	}
	return limit, nil
}
func normalizeQuery(q Query) (Query, error) {
	switch q.Scope.Kind {
	case "":
		if q.Scope.ID != "" {
			return q, ErrInvalidQuery
		}
	case "children": // empty parent denotes roots
	case "project", "conversation":
		if q.Scope.ID == "" {
			return q, ErrInvalidQuery
		}
	default:
		return q, ErrInvalidQuery
	}
	if q.Status == "all" {
		q.Status = ""
	}
	if q.Archived == "all" {
		q.Archived = ""
	}
	if q.Status != "" && q.Status != "live" && q.Status != "closed" {
		return q, ErrInvalidQuery
	}
	if q.Archived != "" && q.Archived != "hide" && q.Archived != "only" {
		return q, ErrInvalidQuery
	}
	limit, err := pageLimit(q.Limit)
	q.Limit = limit
	return q, err
}
func (s *Store) cursorOffset(raw string, key queryKey, total int) (int, error) {
	if raw == "" {
		return 0, nil
	}
	if len(raw) > 4096 {
		return 0, ErrInvalidQuery
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return 0, ErrInvalidQuery
	}
	var cursor queryCursor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return 0, ErrInvalidQuery
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return 0, ErrInvalidQuery
	}
	if cursor.Version != 1 || cursor.Owner == "" || cursor.Revision == 0 || cursor.Scope != key.Scope || cursor.Status != key.Status || cursor.Archived != key.Archived || cursor.Offset <= 0 {
		return 0, ErrInvalidQuery
	}
	if cursor.Owner != s.readIndex.nonce || cursor.Revision != s.readIndex.versions[key] {
		return 0, ErrStaleCursor
	}
	if cursor.Offset >= total {
		return 0, ErrInvalidQuery
	}
	return cursor.Offset, nil
}
func (s *Store) nextCursor(key queryKey, offset, total int) string {
	if offset >= total {
		return ""
	}
	raw, _ := json.Marshal(queryCursor{Version: 1, Owner: s.readIndex.nonce, Revision: s.readIndex.versions[key], Scope: key.Scope, Status: key.Status, Archived: key.Archived, Offset: offset})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func (s *Store) Query(q Query) (Page, error) {
	q, err := normalizeQuery(q)
	if err != nil {
		return Page{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := queryKey{q.Scope, q.Status, q.Archived}
	ids := s.readIndex.ordered[key]
	start, err := s.cursorOffset(q.Cursor, key, len(ids))
	if err != nil {
		return Page{}, err
	}
	end := min(start+q.Limit, len(ids))
	page := Page{Items: make([]Header, 0, end-start), Total: len(ids), NextCursor: s.nextCursor(key, end, len(ids))}
	for _, id := range ids[start:end] {
		h, _ := s.headerLocked(id)
		page.Items = append(page.Items, h)
	}
	return page, nil
}
func (s *Store) QueryAttempts(id, cursor string, limit int) (AccountingPage, error) {
	limit, err := pageLimit(limit)
	if err != nil {
		return AccountingPage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data.Tasks[id]
	if !ok {
		return AccountingPage{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	key := accountingQueryKey(id)
	start, err := s.cursorOffset(cursor, key, len(t.Attempts))
	if err != nil {
		return AccountingPage{}, err
	}
	end := min(start+limit, len(t.Attempts))
	page := AccountingPage{Items: make([]AccountingRow, 0, end-start), Total: len(t.Attempts), NextCursor: s.nextCursor(key, end, len(t.Attempts))}
	for offset := start; offset < end; offset++ {
		i := len(t.Attempts) - 1 - offset
		row := t.Attempts[i]
		if row.UsageKnown != nil {
			known := *row.UsageKnown
			row.UsageKnown = &known
		}
		page.Items = append(page.Items, AccountingRow{Index: i, Attempt: row})
	}
	return page, nil
}
