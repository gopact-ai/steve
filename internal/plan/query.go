package plan

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
)

const MaxQueryLimit = 100

var ErrInvalidQuery = errors.New("invalid plan query or cursor")
var ErrStaleCursor = errors.New("plan query changed; refresh from the first page")

type Query struct {
	TaskID    string `json:"task_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}
type Page struct {
	Items      []Plan `json:"items"`
	Total      int    `json:"total"`
	NextCursor string `json:"next_cursor,omitempty"`
}
type queryCursor struct {
	Version   int    `json:"v"`
	Owner     string `json:"owner"`
	Revision  uint64 `json:"revision"`
	TaskID    string `json:"task_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Offset    int    `json:"offset"`
}

// Query holds one owner read boundary for scope selection, order, payload and
// cursor. A successful mutation in the selected scope expires the cursor
// rather than skipping keys; unrelated plans do not interrupt pagination.
func (s *Store) Query(q Query) (Page, error) {
	if q.Limit < 0 || q.Limit > MaxQueryLimit || len(q.Cursor) > 4096 {
		return Page{}, ErrInvalidQuery
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := planScope{TaskID: q.TaskID, ProjectID: q.ProjectID}
	ids := s.readIndex.ordered[scope]
	start := 0
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(q.Cursor)
		if err != nil {
			return Page{}, ErrInvalidQuery
		}
		var c queryCursor
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return Page{}, ErrInvalidQuery
		}
		if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
			return Page{}, ErrInvalidQuery
		}
		if c.Version != 1 || c.Owner == "" || c.Revision == 0 || c.TaskID != q.TaskID || c.ProjectID != q.ProjectID || c.Offset <= 0 {
			return Page{}, ErrInvalidQuery
		}
		if c.Owner != s.readIndex.nonce || c.Revision != s.readIndex.versions[scope] {
			return Page{}, ErrStaleCursor
		}
		if c.Offset >= len(ids) {
			return Page{}, ErrInvalidQuery
		}
		start = c.Offset
	}
	end := min(start+q.Limit, len(ids))
	page := Page{Items: make([]Plan, 0, end-start), Total: len(ids)}
	for _, id := range ids[start:end] {
		p, _ := latest(s.data, id)
		page.Items = append(page.Items, clonePlan(p))
	}
	if end < len(ids) {
		raw, _ := json.Marshal(queryCursor{Version: 1, Owner: s.readIndex.nonce, Revision: s.readIndex.versions[scope], TaskID: q.TaskID, ProjectID: q.ProjectID, Offset: end})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
