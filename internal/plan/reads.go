package plan

import (
	"crypto/rand"
	"fmt"
	"slices"
	"sort"
)

// Startup already loads every revision. Validate the identities used by the
// read indexes there, rather than silently omitting a malformed historical row.
func validateReadState(d data) error {
	for id, revisions := range d.Plans {
		if id == "" || len(revisions) == 0 {
			return fmt.Errorf("plan: empty history %q", id)
		}
		for i, p := range revisions {
			if p.ID != id || p.Rev != i+1 {
				return fmt.Errorf("plan: invalid revision %s/%d", id, i+1)
			}
		}
	}
	for taskID, id := range d.ByTask {
		p, exists := latest(d, id)
		if taskID == "" || !exists || p.TaskID != taskID {
			return fmt.Errorf("plan: invalid task binding %q", taskID)
		}
	}
	return nil
}

type planScope struct{ TaskID, ProjectID string }
type readIndex struct {
	nonce    string
	revision uint64
	ordered  map[planScope][]string
	live     map[string]bool
}

func latest(data data, id string) (Plan, bool) {
	list := data.Plans[id]
	if len(list) == 0 {
		return Plan{}, false
	}
	return list[len(list)-1], true
}
func planBefore(a, b Plan) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID > b.ID
	}
	return a.CreatedAt.After(b.CreatedAt)
}
func planScopes(p Plan) []planScope {
	scopes := []planScope{{}}
	if p.TaskID != "" {
		scopes = append(scopes, planScope{TaskID: p.TaskID})
	}
	if p.ProjectID != "" {
		scopes = append(scopes, planScope{ProjectID: p.ProjectID})
	}
	if p.TaskID != "" && p.ProjectID != "" {
		scopes = append(scopes, planScope{TaskID: p.TaskID, ProjectID: p.ProjectID})
	}
	return scopes
}
func planPosition(ids []string, p Plan, data data) int {
	return sort.Search(len(ids), func(i int) bool { item, _ := latest(data, ids[i]); return !planBefore(item, p) })
}
func (s *Store) rebuildReadIndexLocked() {
	r := readIndex{nonce: rand.Text(), revision: 1, ordered: map[planScope][]string{}, live: map[string]bool{}}
	ids := make([]string, 0, len(s.data.Plans))
	for id := range s.data.Plans {
		if _, ok := latest(s.data, id); ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := latest(s.data, ids[i])
		b, _ := latest(s.data, ids[j])
		return planBefore(a, b)
	})
	for _, id := range ids {
		p, _ := latest(s.data, id)
		if !p.Complete() {
			r.live[id] = true
		}
		for _, scope := range planScopes(p) {
			r.ordered[scope] = append(r.ordered[scope], id)
		}
	}
	s.readIndex = r
}

// Called only with IDs known to the successful mutation. Progress never
// rebuilds historical indexes or republishes the unchanged task binding set.
func (s *Store) updateReadIndexLocked(next data, ids []string) {
	r := &s.readIndex
	bindings := map[string]bool{}
	changedOrder := map[string]bool{}
	for _, id := range ids {
		old, had := latest(s.data, id)
		p, has := latest(next, id)
		if had && old.TaskID != "" {
			bindings[old.TaskID] = len(r.ordered[planScope{TaskID: old.TaskID}]) > 0
		}
		if has && p.TaskID != "" {
			bindings[p.TaskID] = len(r.ordered[planScope{TaskID: p.TaskID}]) > 0
		}
		if had && has && old.TaskID == p.TaskID && old.ProjectID == p.ProjectID && old.CreatedAt.Equal(p.CreatedAt) {
			continue
		}
		changedOrder[id] = true
		if had {
			for _, scope := range planScopes(old) {
				items := r.ordered[scope]
				at := planPosition(items, old, s.data)
				if at < len(items) && items[at] == id {
					items = slices.Delete(items, at, at+1)
				}
				if len(items) == 0 {
					delete(r.ordered, scope)
				} else {
					r.ordered[scope] = items
				}
			}
		}
	}
	ordered := slices.Clone(ids)
	sort.Slice(ordered, func(i, j int) bool {
		a, hasA := latest(next, ordered[i])
		b, hasB := latest(next, ordered[j])
		if !hasA || !hasB {
			return hasA
		}
		return planBefore(a, b)
	})
	for _, id := range ordered {
		p, has := latest(next, id)
		if !has || p.Complete() {
			delete(r.live, id)
		} else {
			r.live[id] = true
		}
		if !has || !changedOrder[id] {
			continue
		}
		for _, scope := range planScopes(p) {
			items := r.ordered[scope]
			at := planPosition(items, p, next)
			r.ordered[scope] = slices.Insert(items, at, id)
		}
	}
	r.revision++
	membershipChanged := false
	for id, existed := range bindings {
		membershipChanged = membershipChanged || existed != (len(r.ordered[planScope{TaskID: id}]) > 0)
	}
	if membershipChanged {
		s.publishTaskProjectionLocked()
	}
}

// SetTaskProjection wires the derived plan-in-tree summary once at assembly.
// The callback runs after durable success under the plan lock and must not
// reenter this store. Step progress does not change or republish bindings.
func (s *Store) SetTaskProjection(project func([]string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectTasks = project
	s.publishTaskProjectionLocked()
}
func (s *Store) publishTaskProjectionLocked() {
	if s.projectTasks == nil {
		return
	}
	ids := []string{}
	for scope := range s.readIndex.ordered {
		if scope.TaskID != "" && scope.ProjectID == "" {
			ids = append(ids, scope.TaskID)
		}
	}
	sort.Strings(ids)
	s.projectTasks(ids)
}
func (s *Store) Live() []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Plan, 0, len(s.readIndex.live))
	for id := range s.readIndex.live {
		p, _ := latest(s.data, id)
		out = append(out, clonePlan(p))
	}
	sort.Slice(out, func(i, j int) bool { return planBefore(out[i], out[j]) })
	return out
}

// ForTasks returns only each task's current bound plan. Older plan identities
// and revisions stay accessible through Query and Latest, not the base state.
func (s *Store) ForTasks(ids []string) []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []Plan{}
	for _, taskID := range ids {
		id := s.data.ByTask[taskID]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if p, ok := latest(s.data, id); ok {
			out = append(out, clonePlan(p))
		}
	}
	return out
}
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.readIndex.ordered[planScope{}])
}
