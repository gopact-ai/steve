package plan

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/gopact-ai/steve/internal/ledger"
)

type ProjectTransfer struct {
	Project string            `json:"project"`
	Plans   map[string][]Plan `json:"plans"`
}

func ExportProject(doc ledger.Doc, project string) (ProjectTransfer, error) {
	out := ProjectTransfer{Project: project, Plans: map[string][]Plan{}}
	raw, ok, err := doc.Load()
	if err != nil || !ok {
		return out, err
	}
	var d data
	if err := json.Unmarshal(raw, &d); err != nil {
		return out, err
	}
	for id, revs := range d.Plans {
		for _, p := range revs {
			if p.ProjectID == project {
				out.Plans[id] = revs
				break
			}
		}
	}
	return out, nil
}
func ImportProject(doc ledger.Doc, in ProjectTransfer) error {
	raw, ok, err := doc.Load()
	if err != nil {
		return err
	}
	d := data{NextID: 1, Plans: map[string][]Plan{}, ByTask: map[string]string{}}
	if ok {
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
	}
	if d.Plans == nil {
		d.Plans = map[string][]Plan{}
	}
	if d.ByTask == nil {
		d.ByTask = map[string]string{}
	}
	for id, revs := range in.Plans {
		if old, ok := d.Plans[id]; ok && !reflect.DeepEqual(old, revs) {
			return fmt.Errorf("plan ID collision %s", id)
		}
		for _, p := range revs {
			if p.ID != id || p.ProjectID != in.Project {
				return fmt.Errorf("cross-project plan %s", id)
			}
			if old, ok := d.ByTask[p.TaskID]; ok && old != id {
				return fmt.Errorf("plan task collision %s", p.TaskID)
			}
			d.ByTask[p.TaskID] = id
		}
		d.Plans[id] = revs
		n, err := strconv.Atoi(id)
		if err == nil && n >= d.NextID {
			d.NextID = n + 1
		}
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	return doc.Save(raw)
}

func RemapResult(r *StepResult, m ledger.TransferIDs) {
	if r == nil {
		return
	}
	r.TaskID = m.Task(r.TaskID)
	if r.ExecutionToken != nil {
		r.ExecutionToken.TaskID = m.Task(r.ExecutionToken.TaskID)
	}
	for i := range r.Refs {
		if r.Refs[i].Kind == "task" {
			r.Refs[i].Value = m.Task(r.Refs[i].Value)
		}
	}
}
func (in *ProjectTransfer) Remap(m ledger.TransferIDs) {
	all := map[string][]Plan{}
	for id, revs := range in.Plans {
		for i := range revs {
			revs[i].ID = m.Plan(id)
			revs[i].TaskID = m.Task(revs[i].TaskID)
			for j := range revs[i].Steps {
				RemapResult(revs[i].Steps[j].Result, m)
			}
		}
		all[m.Plan(id)] = revs
	}
	in.Plans = all
}
