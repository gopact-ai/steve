package task

import (
	"crypto/rand"
	"slices"
	"sort"
)

type treeSummary struct{ open, blockers, plans int }

func localBlocker(t *Task) int {
	if t.Settled() {
		return 0
	}
	if !t.State.Terminal() {
		return 1
	}
	if t.State == StateCancelled && t.Result == nil && t.Delivery == nil {
		return 0
	}
	if t.Result == nil || t.Delivery == nil || t.Delivery.State != DeliveryDelivered {
		return 1
	}
	return 0
}
func scopesOf(t *Task) []Scope {
	scopes := []Scope{{}, {Kind: "children", ID: t.Parent}}
	if t.ProjectID != "" {
		scopes = append(scopes, Scope{Kind: "project", ID: t.ProjectID})
	}
	if t.Channel != "" {
		scopes = append(scopes, Scope{Kind: "conversation", ID: t.Channel})
	}
	return scopes
}
func keysOf(t *Task, meta Meta, summary ReadSummary) []queryKey {
	status := "closed"
	if actionable(*t, summary) {
		status = "live"
	}
	archive := "hide"
	if meta.ArchivedAt != nil {
		archive = "only"
	}
	keys := make([]queryKey, 0, 16)
	for _, scope := range scopesOf(t) {
		for _, state := range []string{"", status} {
			for _, archived := range []string{"", archive} {
				keys = append(keys, queryKey{scope, state, archived})
			}
		}
	}
	return keys
}
func (r *readIndex) count(t *Task, summary ReadSummary, delta int) {
	for _, scope := range scopesOf(t) {
		c := r.counts[scope]
		c.Total += delta
		if actionable(*t, summary) {
			c.Live += delta
		} else {
			c.Closed += delta
		}
		if t.Parent == "" {
			c.Roots += delta
			switch t.State {
			case StateDone:
				c.CompletedRoots += delta
			case StateCancelled:
				c.CancelledRoots += delta
			case StatePaused:
				c.PausedRoots += delta
			}
		}
		if c.Total == 0 {
			delete(r.counts, scope)
		} else {
			r.counts[scope] = c
		}
	}
}
func (r *readIndex) addTree(tasks map[string]*Task, id string, delta treeSummary, affected map[string]bool) {
	// Stored tasks form a forest. Guard malformed offline fixtures from cycles.
	visited := map[string]bool{}
	for id != "" && !visited[id] {
		t := tasks[id]
		if t == nil {
			break
		}
		visited[id] = true
		sum := r.trees[id]
		sum.open += delta.open
		sum.blockers += delta.blockers
		sum.plans += delta.plans
		r.trees[id] = sum
		if affected != nil {
			affected[id] = true
		}
		id = t.Parent
	}
}
func (r *readIndex) refreshTree(tasks map[string]*Task, id string) {
	t := tasks[id]
	if t == nil {
		return
	}
	summary := r.summaries[id]
	tree := r.trees[id]
	summary.CanComplete = t.Parent == "" && t.Origin == "" && t.PreparedPlan == nil &&
		(t.State == StateRunning || t.State == StateReview) && tree.open == 0 && tree.blockers-localBlocker(t) == 0
	summary.PlanInTree = tree.plans > 0
	summary.Children = len(r.ordered[queryKey{Scope: Scope{Kind: "children", ID: id}}])
	r.summaries[id] = summary
}
func rowSummary(sum *ReadSummary, row Attempt, sign int) {
	sum.Tokens = sum.Tokens.Add(Tokens{Input: int64(sign) * row.Tokens.Input, Output: int64(sign) * row.Tokens.Output,
		CachedRead: int64(sign) * row.Tokens.CachedRead, CachedWrite: int64(sign) * row.Tokens.CachedWrite, Total: int64(sign) * row.Tokens.Total})
	if row.Open() {
		sum.OpenExecutions += sign
	} else {
		sum.Seconds += int64(sign) * int64(row.EndedAt.Sub(row.StartedAt).Seconds())
	}
}
func rowMembership(rows []int, index int, present bool) []int {
	at, found := slices.BinarySearch(rows, index)
	if present && !found {
		return slices.Insert(rows, at, index)
	}
	if !present && found {
		return slices.Delete(rows, at, at+1)
	}
	return rows
}

// Only startup constructs the full index. Committed record diffs maintain it
// incrementally; no read and no ordinary mutation sorts the entire history.
func (s *Store) rebuildReadIndexLocked() {
	old := s.readIndex
	r := readIndex{nonce: old.nonce, revision: old.revision + 1, planTasks: old.planTasks,
		ordered: map[queryKey][]string{}, summaries: map[string]ReadSummary{}, counts: map[Scope]Counts{},
		trees: map[string]treeSummary{}, models: map[string][]int{}, primary: map[string][]int{}, primaryRows: map[string][]int{}}
	if r.nonce == "" {
		r.nonce = rand.Text()
	}
	ids := make([]string, 0, len(s.data.Tasks))
	for id, t := range s.data.Tasks {
		ids = append(ids, id)
		summary := ReadSummary{Attempts: len(t.Attempts)}
		for i, row := range t.Attempts {
			rowSummary(&summary, row, 1)
			if row.Model != "" {
				r.models[id] = append(r.models[id], i)
				summary.Model = row.Model
			}
			if !row.Independent {
				r.primaryRows[id] = append(r.primaryRows[id], i)
			}
		}
		r.summaries[id] = summary
		r.updatePrimary(t)
		plans := 0
		if r.planTasks[id] {
			plans = 1
		}
		r.addTree(s.data.Tasks, id, treeSummary{summary.OpenExecutions, localBlocker(t), plans}, nil)
	}
	sort.Slice(ids, func(i, j int) bool { return taskBefore(s.data.Tasks[ids[i]], s.data.Tasks[ids[j]]) })
	for _, id := range ids {
		t := s.data.Tasks[id]
		summary := r.summaries[id]
		for _, key := range keysOf(t, s.data.Meta[id], summary) {
			r.ordered[key] = append(r.ordered[key], id)
		}
		r.count(t, summary, 1)
	}
	for _, id := range ids {
		r.refreshTree(s.data.Tasks, id)
	}
	s.readIndex = r
}

func orderedPosition(ids []string, t *Task, tasks map[string]*Task) int {
	return sort.Search(len(ids), func(i int) bool { return !taskBefore(tasks[ids[i]], t) })
}
func (r *readIndex) remove(t *Task, meta Meta, summary ReadSummary, tasks map[string]*Task) {
	for _, key := range keysOf(t, meta, summary) {
		ids := r.ordered[key]
		at := orderedPosition(ids, t, tasks)
		if at < len(ids) && ids[at] == t.ID {
			ids = slices.Delete(ids, at, at+1)
		}
		if len(ids) == 0 {
			delete(r.ordered, key)
		} else {
			r.ordered[key] = ids
		}
	}
	r.count(t, summary, -1)
}
func (r *readIndex) insert(t *Task, meta Meta, summary ReadSummary, tasks map[string]*Task) {
	for _, key := range keysOf(t, meta, summary) {
		ids := r.ordered[key]
		at := orderedPosition(ids, t, tasks)
		r.ordered[key] = slices.Insert(ids, at, t.ID)
	}
	r.count(t, summary, 1)
}

// Own accounting is adjusted only for changed rows. Subtree blockers and
// plan presence are propagated only through affected ancestor paths.
func (s *Store) updateReadIndexLocked(next data, changes []recordChange) {
	if len(changes) == 0 {
		return
	}
	r := &s.readIndex
	changed := map[string][]int{}
	for _, change := range changes {
		switch change.kind {
		case taskKind, taskMetaKind:
			if _, ok := changed[change.id]; !ok {
				changed[change.id] = nil
			}
		case taskAttemptKind:
			if row, ok := change.value.(attemptRecord); ok {
				changed[row.TaskID] = append(changed[row.TaskID], row.Index)
			}
		}
	}
	// Every changed key is removed using the old order before any replacement
	// key is inserted. Unchanged members preserve their backing index arrays.
	affected := map[string]bool{}
	for id := range changed {
		if old := s.data.Tasks[id]; old != nil {
			summary := r.summaries[id]
			r.remove(old, s.data.Meta[id], summary, s.data.Tasks)
			plans := 0
			if r.planTasks[id] {
				plans = 1
			}
			r.addTree(s.data.Tasks, id, treeSummary{-summary.OpenExecutions, -localBlocker(old), -plans}, affected)
			if old.Parent != "" {
				affected[old.Parent] = true
			}
		}
	}
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := next.Tasks[ids[i]], next.Tasks[ids[j]]
		if a == nil || b == nil {
			return a != nil
		}
		return taskBefore(a, b)
	})
	for _, id := range ids {
		rows := changed[id]
		t := next.Tasks[id]
		if t == nil {
			delete(r.summaries, id)
			delete(r.trees, id)
			delete(r.models, id)
			delete(r.primary, id)
			delete(r.primaryRows, id)
			continue
		}
		sum := r.summaries[id]
		sum.Attempts = len(t.Attempts)
		for _, i := range rows {
			if old := s.data.Tasks[id]; old != nil && i < len(old.Attempts) {
				rowSummary(&sum, old.Attempts[i], -1)
			}
			row := t.Attempts[i]
			rowSummary(&sum, row, 1)
			r.models[id] = rowMembership(r.models[id], i, row.Model != "")
			r.primaryRows[id] = rowMembership(r.primaryRows[id], i, !row.Independent)
		}
		if len(r.models[id]) == 0 {
			delete(r.models, id)
			sum.Model = ""
		} else {
			sum.Model = t.Attempts[r.models[id][len(r.models[id])-1]].Model
		}
		r.updatePrimary(t)
		r.summaries[id] = sum
		plans := 0
		if r.planTasks[id] {
			plans = 1
		}
		r.addTree(next.Tasks, id, treeSummary{sum.OpenExecutions, localBlocker(t), plans}, affected)
		r.insert(t, next.Meta[id], sum, next.Tasks)
		if t.Parent != "" {
			affected[t.Parent] = true
		}
	}
	for id := range affected {
		r.refreshTree(next.Tasks, id)
	}
	r.revision++
}

// SetPlanBindings is a derived owner projection, not storage authority. Only
// added/removed bindings update ancestors; the admission guard stays same-Tx.
func (s *Store) SetPlanBindings(taskIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		if id != "" {
			next[id] = true
		}
	}
	r := &s.readIndex
	affected := map[string]bool{}
	for id := range r.planTasks {
		if !next[id] {
			r.addTree(s.data.Tasks, id, treeSummary{plans: -1}, affected)
		}
	}
	for id := range next {
		if !r.planTasks[id] {
			r.addTree(s.data.Tasks, id, treeSummary{plans: 1}, affected)
		}
	}
	r.planTasks = next
	for id := range affected {
		r.refreshTree(s.data.Tasks, id)
	}
	if len(affected) > 0 {
		r.revision++
	}
}

// RecoveryCandidate names one exact outstanding primary accounting row. The
// absolute index survives omission of historical rows from Task.Attempts.
type RecoveryCandidate struct {
	Task    Task
	Index   int
	Attempt Attempt
}

func (s *Store) OpenPrimaryAccounting() []RecoveryCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []RecoveryCandidate{}
	for id, rows := range s.readIndex.primary {
		head, _ := s.headerLocked(id)
		for _, i := range rows {
			row := s.data.Tasks[id].Attempts[i]
			if row.UsageKnown != nil {
				value := *row.UsageKnown
				row.UsageKnown = &value
			}
			out = append(out, RecoveryCandidate{Task: head.Task, Index: i, Attempt: row})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Task.ID == out[j].Task.ID {
			return out[i].Index > out[j].Index
		}
		return taskBefore(&out[i].Task, &out[j].Task)
	})
	return out
}

func (r *readIndex) updatePrimary(t *Task) {
	rows := r.primaryRows[t.ID]
	if len(rows) == 0 {
		delete(r.primaryRows, t.ID)
	}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		if t.Attempts[last].Open() {
			r.primary[t.ID] = []int{last}
			return
		}
	}
	delete(r.primary, t.ID)
}
