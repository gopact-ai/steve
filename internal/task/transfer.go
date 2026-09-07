package task

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/gopact-ai/steve/internal/ledger"
)

type ProjectTransfer struct {
	Project string           `json:"project"`
	Tasks   map[string]*Task `json:"tasks"`
	Meta    map[string]Meta  `json:"meta"`
}

func ExportProject(doc ledger.Doc, project string) (ProjectTransfer, error) {
	out := ProjectTransfer{Project: project, Tasks: map[string]*Task{}, Meta: map[string]Meta{}}
	raw, ok, err := doc.Load()
	if err != nil || !ok {
		return out, err
	}
	var d data
	if err := json.Unmarshal(raw, &d); err != nil {
		return out, err
	}
	for id, t := range d.Tasks {
		if t.ProjectID == project {
			out.Tasks[id] = t
			if m, ok := d.Meta[id]; ok {
				out.Meta[id] = m
			}
		}
	}
	for _, t := range out.Tasks {
		if t.Parent != "" && out.Tasks[t.Parent] == nil {
			return out, fmt.Errorf("cross-project task ancestry %s", t.ID)
		}
	}
	return out, nil
}
func ImportProject(doc ledger.Doc, in ProjectTransfer) error {
	raw, ok, err := doc.Load()
	if err != nil {
		return err
	}
	d := data{NextID: 1, Tasks: map[string]*Task{}, Meta: map[string]Meta{}}
	if ok {
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
	}
	if d.Tasks == nil {
		d.Tasks = map[string]*Task{}
	}
	if d.Meta == nil {
		d.Meta = map[string]Meta{}
	}
	for id, t := range in.Tasks {
		if t == nil || t.ID != id || t.ProjectID != in.Project {
			return fmt.Errorf("invalid project task %s", id)
		}
		if old, ok := d.Tasks[id]; ok && !reflect.DeepEqual(old, t) {
			return fmt.Errorf("task ID collision %s", id)
		}
		d.Tasks[id] = t
		n, err := strconv.Atoi(id)
		if err == nil && n >= d.NextID {
			d.NextID = n + 1
		}
	}
	for id, m := range in.Meta {
		if in.Tasks[id] == nil {
			return fmt.Errorf("orphan task metadata %s", id)
		}
		if old, ok := d.Meta[id]; ok && !reflect.DeepEqual(old, m) {
			return fmt.Errorf("task metadata collision %s", id)
		}
		d.Meta[id] = m
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	return doc.Save(raw)
}

func (in *ProjectTransfer) Remap(m ledger.TransferIDs) {
	tasks := map[string]*Task{}
	meta := map[string]Meta{}
	for id, t := range in.Tasks {
		t.ID = m.Task(id)
		t.Parent = m.Task(t.Parent)
		t.Channel = m.Conversation(t.Channel)
		t.Origin = m.Key(t.Origin)
		if t.Result != nil {
			for i, ref := range t.Result.Refs {
				t.Result.Refs[i] = ledger.RemapTaskReference(ref, m.Task)
			}
		}
		if t.Delivery != nil {
			t.Delivery.Key = m.Key(t.Delivery.Key)
		}
		tasks[t.ID] = t
		if value, ok := in.Meta[id]; ok {
			meta[t.ID] = value
		}
	}
	in.Tasks, in.Meta = tasks, meta
}

// FreezeProject seals dispatchable source tasks after their pre-freeze state
// has been bundled. Historical attempts, costs and results remain intact.
func FreezeProject(doc ledger.Doc, project string) error {
	raw, ok, err := doc.Load()
	if err != nil || !ok {
		return err
	}
	var d data
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	for _, t := range d.Tasks {
		if t.ProjectID == project && !t.State.Terminal() {
			t.State = StateCancelled
			t.ExecutionEpoch++
		}
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	return doc.Save(raw)
}
