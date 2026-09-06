package ledger

import "strings"

// RemapTaskReference handles the explicit "task <id>" protocol reference.
// It is used only for structured result refs, never answer or goal prose.
func RemapTaskReference(ref string, taskID func(string) string) string {
	if id, ok := strings.CutPrefix(ref, "task "); ok && id != "" && taskID != nil {
		return "task " + taskID(id)
	}
	return ref
}

// TransferIDs describes explicit identity relocation. Content fields are not
// interpreted or rewritten by this map.
type TransferIDs struct {
	Namespace                                          string
	Tasks, Plans, Schedules, Conversations, Operations map[string]string
}

func mapped(m map[string]string, id string) string {
	if next, ok := m[id]; ok {
		return next
	}
	return id
}
func (m TransferIDs) Task(id string) string         { return m.scoped(m.Tasks, id) }
func (m TransferIDs) Plan(id string) string         { return m.scoped(m.Plans, id) }
func (m TransferIDs) Schedule(id string) string     { return m.scoped(m.Schedules, id) }
func (m TransferIDs) Conversation(id string) string { return mapped(m.Conversations, id) }
func (m TransferIDs) Operation(id string) string    { return mapped(m.Operations, id) }
func (m TransferIDs) Key(key string) string {
	for _, prefix := range []string{"deliver:", "delegate:"} {
		if id, ok := strings.CutPrefix(key, prefix); ok {
			return prefix + m.Task(id)
		}
	}
	if tail, ok := strings.CutPrefix(key, "schedule:"); ok {
		id, rest, found := strings.Cut(tail, ":")
		if found {
			return "schedule:" + m.Schedule(id) + ":" + rest
		}
		return "schedule:" + m.Schedule(tail)
	}
	return key
}
func (m TransferIDs) Name(name string) string {
	if tail, ok := strings.CutPrefix(name, "steve/"); ok {
		id, rest, found := strings.Cut(tail, "/")
		if found {
			return "steve/" + m.Task(id) + "/" + rest
		}
	}
	if tail, ok := strings.CutPrefix(name, "conversation/"); ok {
		if id, ok := strings.CutSuffix(tail, "/project"); ok {
			return "conversation/" + m.Conversation(id) + "/project"
		}
	}
	return name
}

// RemapEnvelopes rewrites only ledger-owned IDs; domain payloads must be
// rewritten by their respective domain before this method is called.
func (f *TransferFacts) RemapEnvelopes(m TransferIDs) {
	for i := range f.Operations {
		f.Operations[i].ID = m.Operation(f.Operations[i].ID)
	}
	for i := range f.Events {
		f.Events[i].OperationID = m.Operation(f.Events[i].OperationID)
	}
	for i := range f.Effects {
		f.Effects[i].Effect.Operation = m.Operation(f.Effects[i].Effect.Operation)
	}
	for i := range f.Names {
		f.Names[i].Name = m.Name(f.Names[i].Name)
	}
}

func (m TransferIDs) scoped(values map[string]string, id string) string {
	if next, ok := values[id]; ok {
		return next
	}
	if id == "" || m.Namespace == "" {
		return id
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return id
		}
	}
	return m.Namespace + "~" + id
}
