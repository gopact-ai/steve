package schedule

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type ProjectTransfer struct {
	Project string             `json:"project"`
	Jobs    map[string]*Job    `json:"jobs"`
	Firings map[string]*Firing `json:"firings"`
}

func ExportProject(doc ledger.Doc, project string) (ProjectTransfer, error) {
	out := ProjectTransfer{Project: project, Jobs: map[string]*Job{}, Firings: map[string]*Firing{}}
	raw, ok, err := doc.Load()
	if err != nil || !ok {
		return out, err
	}
	var d data
	if err := json.Unmarshal(raw, &d); err != nil {
		return out, err
	}
	for id, j := range d.Jobs {
		if j.ProjectID == project {
			out.Jobs[id] = j
		}
	}
	for key, f := range d.Firings {
		if f.ProjectID == project {
			out.Firings[key] = f
		}
	}
	return out, nil
}
func ImportProject(doc ledger.Doc, in ProjectTransfer) error {
	raw, ok, err := doc.Load()
	if err != nil {
		return err
	}
	d := data{NextID: 1, Jobs: map[string]*Job{}, Firings: map[string]*Firing{}}
	if ok {
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
	}
	if d.Jobs == nil {
		d.Jobs = map[string]*Job{}
	}
	if d.Firings == nil {
		d.Firings = map[string]*Firing{}
	}
	for id, j := range in.Jobs {
		if j == nil || j.ID != id || j.ProjectID != in.Project {
			return fmt.Errorf("invalid schedule %s", id)
		}
		if old, ok := d.Jobs[id]; ok && !reflect.DeepEqual(old, j) {
			return fmt.Errorf("schedule collision %s", id)
		}
		d.Jobs[id] = j
		n, err := strconv.Atoi(id)
		if err == nil && n >= d.NextID {
			d.NextID = n + 1
		}
	}
	for key, f := range in.Firings {
		if f.ProjectID != in.Project || f.Key != key {
			return fmt.Errorf("invalid schedule firing")
		}
		if old, ok := d.Firings[key]; ok && !reflect.DeepEqual(old, f) {
			return fmt.Errorf("firing collision %s", key)
		}
		d.Firings[key] = f
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	return doc.Save(raw)
}

func (in *ProjectTransfer) Remap(m ledger.TransferIDs) {
	jobs := map[string]*Job{}
	for id, j := range in.Jobs {
		j.ID = m.Schedule(id)
		j.ConversationID = m.Conversation(j.ConversationID)
		jobs[j.ID] = j
	}
	firings := map[string]*Firing{}
	for key, f := range in.Firings {
		f.ID = m.Schedule(f.ID)
		f.ConversationID = m.Conversation(f.ConversationID)
		f.Key = m.Key(key)
		firings[f.Key] = f
	}
	in.Jobs, in.Firings = jobs, firings
}

func FreezeProject(doc ledger.Doc, project string) error {
	raw, ok, err := doc.Load()
	if err != nil || !ok {
		return err
	}
	var d data
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	for _, j := range d.Jobs {
		if j.ProjectID == project {
			j.NextAt = time.Time{}
		}
	}
	for _, f := range d.Firings {
		if f.ProjectID == project && (f.State == FiringPending || f.State == FiringDispatching || f.State == FiringUnknown) {
			f.State = FiringCancelled
			f.Error = "project migrated from this hub"
		}
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	return doc.Save(raw)
}
