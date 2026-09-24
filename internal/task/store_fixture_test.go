package task

// clone deep-copies the committed data so a test can build a whole
// candidate state by hand.
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

// replaceData writes a whole candidate state through the store's write
// path: every task and metadata entry it holds or drops is part of the draft.
func (s *Store) replaceData(next data) error {
	return s.replaceLocked(draftOf(&s.data, next))
}

func draftOf(base *data, next data) *draft {
	d := newDraft(base)
	d.NextID = next.NextID
	for id, t := range next.Tasks {
		d.tasks[id] = t
	}
	for id := range base.Tasks {
		if _, ok := next.Tasks[id]; !ok {
			d.tasks[id] = nil
		}
	}
	for id, meta := range next.Meta {
		d.meta[id] = &meta
	}
	for id := range base.Meta {
		if _, ok := next.Meta[id]; !ok {
			d.meta[id] = nil
		}
	}
	return d
}
