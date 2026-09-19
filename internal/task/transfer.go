package task

import (
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

// ExportProjectTx assembles complete task histories and metadata at one
// committed boundary. Only the task owner knows the record layout.
func ExportProjectTx(tx *ledger.Tx, project string) (ProjectTransfer, error) {
	records, err := loadRecordSetTx(tx)
	if err != nil {
		return ProjectTransfer{}, err
	}
	d, _, err := decodeRecords(records)
	if err != nil {
		return ProjectTransfer{}, err
	}
	return exportProject(d, project)
}

func exportProject(d data, project string) (ProjectTransfer, error) {
	out := ProjectTransfer{Project: project, Tasks: map[string]*Task{}, Meta: map[string]Meta{}}
	for id, t := range d.Tasks {
		if t.ProjectID == project {
			out.Tasks[id] = t.clone()
			if meta, ok := d.Meta[id]; ok {
				out.Meta[id] = meta
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

// ValidateProjectImportTx checks exactly the records ImportProjectTx will
// merge, without publishing tasks during the import's filesystem phase.
func ValidateProjectImportTx(tx *ledger.Tx, in ProjectTransfer) error {
	_, _, _, err := projectImportChangesTx(tx, in)
	return err
}

func ImportProjectTx(tx *ledger.Tx, in ProjectTransfer) error {
	changes, nextID, revision, err := projectImportChangesTx(tx, in)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	return writeRecordChangesTx(tx, changes, nextID, revision)
}

func projectImportChangesTx(tx *ledger.Tx, in ProjectTransfer) ([]recordChange, int, uint64, error) {
	records, err := loadRecordSetTx(tx)
	if err != nil {
		return nil, 0, 0, err
	}
	before, revision, err := decodeRecords(records)
	if err != nil {
		return nil, 0, 0, err
	}
	next := data{NextID: before.NextID, Tasks: make(map[string]*Task, len(before.Tasks)), Meta: make(map[string]Meta, len(before.Meta))}
	for id, t := range before.Tasks {
		next.Tasks[id] = t
	}
	for id, m := range before.Meta {
		next.Meta[id] = m
	}
	for id, t := range in.Tasks {
		if t == nil || id == "" || t.ID != id || t.ProjectID != in.Project {
			return nil, 0, 0, fmt.Errorf("invalid project task %s", id)
		}
		if t.Parent != "" && in.Tasks[t.Parent] == nil {
			return nil, 0, 0, fmt.Errorf("cross-project task ancestry %s", id)
		}
		if old, ok := before.Tasks[id]; ok && !reflect.DeepEqual(old, t) {
			return nil, 0, 0, fmt.Errorf("task ID collision %s", id)
		}
		next.Tasks[id] = t
		if n, err := strconv.Atoi(id); err == nil && n >= next.NextID {
			next.NextID = n + 1
		}
	}
	for id, m := range in.Meta {
		if in.Tasks[id] == nil {
			return nil, 0, 0, fmt.Errorf("orphan task metadata %s", id)
		}
		if old, ok := before.Meta[id]; ok && !old.equal(m) {
			return nil, 0, 0, fmt.Errorf("task metadata collision %s", id)
		}
		next.Meta[id] = m
	}
	changes, err := recordChanges(before, next)
	return changes, next.NextID, revision, err
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

// FreezeProjectTx seals exactly the tasks included in the durable bundle.
// A concurrent addition/change refuses release instead of leaving an exported
// task writable or freezing a task whose latest records were never exported.
func FreezeProjectTx(tx *ledger.Tx, exported ProjectTransfer) error {
	records, err := loadRecordSetTx(tx)
	if err != nil {
		return err
	}
	before, revision, err := decodeRecords(records)
	if err != nil {
		return err
	}
	current, err := exportProject(before, exported.Project)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, exported) {
		return fmt.Errorf("%w: tasks changed during export", ledger.ErrConflict)
	}
	var changes []recordChange
	for id, t := range current.Tasks {
		if t.State.Terminal() {
			continue
		}
		t.State = StateCancelled
		t.ExecutionEpoch++
		changes = append(changes, recordChange{kind: taskKind, id: id, value: headOf(t)})
	}
	if len(changes) == 0 {
		return nil
	}
	return writeRecordChangesTx(tx, changes, before.NextID, revision)
}
