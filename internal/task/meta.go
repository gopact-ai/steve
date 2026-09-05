package task

import (
	"fmt"
	"slices"
	"time"
)

// Meta keeps the owner's organization separate from execution: archiving a
// task must not release its conversation, spend its budget, or stop its work.
type Meta struct {
	Title      string     `json:"title,omitempty"`
	Priority   string     `json:"priority,omitempty"`
	Labels     []string   `json:"labels,omitempty"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	Rank       int        `json:"rank,omitempty"`
}

type MetaPatch struct {
	Title    *string
	Priority *string
	Labels   *[]string
	Archived *bool
	Rank     *int
}

func (s *Store) SetMeta(id string, patch MetaPatch) (Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Tasks[id]; !ok {
		return Meta{}, fmt.Errorf("task %s not found", id)
	}
	meta := s.data.Meta[id].clone()
	if patch.Title != nil {
		meta.Title = *patch.Title
	}
	if patch.Priority != nil {
		switch *patch.Priority {
		case "", "normal":
			meta.Priority = "normal"
		case "high", "low":
			meta.Priority = *patch.Priority
		default:
			return Meta{}, fmt.Errorf("invalid task priority %q", *patch.Priority)
		}
	}
	if patch.Labels != nil {
		meta.Labels = slices.Clone(*patch.Labels)
	}
	if patch.Archived != nil {
		if !*patch.Archived {
			meta.ArchivedAt = nil
		} else if meta.ArchivedAt == nil {
			// A retry must preserve when the owner first put the task away.
			now := s.now()
			meta.ArchivedAt = &now
		}
	}
	if patch.Rank != nil {
		meta.Rank = *patch.Rank
	}
	if meta.equal(s.data.Meta[id]) {
		return meta, nil
	}
	next := s.clone()
	next.Meta[id] = meta
	if err := s.replaceLocked(next); err != nil {
		return Meta{}, err
	}
	return meta.clone(), nil
}

func (s *Store) MetaOf(id string) Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Meta[id].clone()
}

func (m Meta) priority() string {
	if m.Priority == "" {
		return "normal"
	}
	return m.Priority
}

func (m Meta) clone() Meta {
	m.Priority = m.priority()
	m.Labels = slices.Clone(m.Labels)
	if m.ArchivedAt != nil {
		at := *m.ArchivedAt
		m.ArchivedAt = &at
	}
	return m
}

func (m Meta) equal(other Meta) bool {
	sameArchive := m.ArchivedAt == nil && other.ArchivedAt == nil ||
		m.ArchivedAt != nil && other.ArchivedAt != nil && m.ArchivedAt.Equal(*other.ArchivedAt)
	return m.Title == other.Title && m.priority() == other.priority() &&
		slices.Equal(m.Labels, other.Labels) && sameArchive && m.Rank == other.Rank
}
