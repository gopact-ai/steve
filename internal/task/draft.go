package task

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
)

// draft is one write's candidate state. It reads through to the committed
// data and holds private copies of only the tasks and metadata the write
// touches, so a write costs what it changes rather than the whole history.
// Committed tasks are never changed in place: a refused write drops its
// draft and memory stays as it was.
type draft struct {
	base   *data
	NextID int
	tasks  map[string]*Task // private copies; nil removes the task
	meta   map[string]*Meta // replacements; nil removes the metadata
}

func newDraft(base *data) *draft {
	return &draft{base: base, NextID: base.NextID, tasks: map[string]*Task{}, meta: map[string]*Meta{}}
}

func (s *Store) draft() *draft { return newDraft(&s.data) }

// edit returns the write's own copy of id, copying the committed task on
// first use, or nil when there is no such task. Every task a write may
// change is reached through edit.
func (d *draft) edit(id string) *Task {
	if t, ok := d.tasks[id]; ok {
		return t
	}
	stored := d.base.Tasks[id]
	if stored == nil {
		return nil
	}
	t := stored.clone()
	d.tasks[id] = t
	return t
}

// find reads id as the write currently sees it, without copying it.
func (d *draft) find(id string) (*Task, bool) {
	if t, ok := d.tasks[id]; ok {
		return t, t != nil
	}
	t, ok := d.base.Tasks[id]
	return t, ok
}

func (d *draft) add(t *Task) { d.tasks[t.ID] = t }

// remove drops the task together with its metadata.
func (d *draft) remove(id string) {
	d.tasks[id] = nil
	d.meta[id] = nil
}

func (d *draft) setMeta(id string, meta Meta) { d.meta[id] = &meta }

// lineage is the task followed by its ancestors, each as the write's own copy.
func (d *draft) lineage(id string) ([]*Task, error) {
	return lineageOf(func(id string) (*Task, bool) {
		t := d.edit(id)
		return t, t != nil
	}, id)
}

// changes lists the records the draft changes, comparing only what it touched.
func (d *draft) changes() ([]recordChange, error) {
	var changes []recordChange
	for id, t := range d.tasks {
		old := d.base.Tasks[id]
		if t == nil {
			if old == nil {
				continue
			}
			if _, kept := d.base.Meta[id]; kept {
				if m, touched := d.meta[id]; !touched || m != nil {
					return nil, fmt.Errorf("task: orphan metadata %s", id)
				}
			}
			changes = append(changes, recordChange{kind: taskKind, id: id}, recordChange{kind: taskAttemptKind, id: attemptPrefix(id), removeAttempts: true})
			continue
		}
		if t.ID != id {
			return nil, fmt.Errorf("task: invalid task %s", id)
		}
		if old != nil && len(old.Attempts) > len(t.Attempts) {
			return nil, errors.New("task: attempt history cannot be truncated")
		}
		if old == nil || !reflect.DeepEqual(headOf(old), headOf(t)) {
			changes = append(changes, recordChange{kind: taskKind, id: id, value: headOf(t)})
		}
		for i, row := range t.Attempts {
			if old == nil || i >= len(old.Attempts) || !reflect.DeepEqual(old.Attempts[i], row) {
				changes = append(changes, recordChange{kind: taskAttemptKind, id: attemptKey(id, i), value: attemptRecord{TaskID: id, Index: i, Attempt: row}})
			}
		}
	}
	for id, meta := range d.meta {
		old, found := d.base.Meta[id]
		if meta == nil {
			if found {
				changes = append(changes, recordChange{kind: taskMetaKind, id: id})
			}
			continue
		}
		if _, ok := d.find(id); !ok {
			return nil, fmt.Errorf("task: orphan metadata %s", id)
		}
		if !found || !meta.equal(old) {
			changes = append(changes, recordChange{kind: taskMetaKind, id: id, value: *meta})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].kind != changes[j].kind {
			return changes[i].kind < changes[j].kind
		}
		return changes[i].id < changes[j].id
	})
	return changes, nil
}

// applyTo installs the draft into the committed data it was drawn from.
func (d *draft) applyTo(target *data) {
	target.NextID = d.NextID
	for id, t := range d.tasks {
		if t == nil {
			delete(target.Tasks, id)
		} else {
			target.Tasks[id] = t
		}
	}
	for id, meta := range d.meta {
		if meta == nil {
			delete(target.Meta, id)
		} else {
			target.Meta[id] = *meta
		}
	}
}

// metaOf is id's metadata as the write currently sees it.
func (d *draft) metaOf(id string) Meta {
	if meta, ok := d.meta[id]; ok {
		if meta == nil {
			return Meta{}
		}
		return *meta
	}
	return d.base.Meta[id]
}
