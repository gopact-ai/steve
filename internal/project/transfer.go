package project

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type ProjectTransfer struct {
	Project       Project              `json:"project"`
	Conversations []string             `json:"conversations"`
	Facts         ledger.TransferFacts `json:"facts"`
}

// InterruptTransferDisclosures preserves the durable request history without
// offering approval of sealed answer bodies that existed only in source memory.
func (in *ProjectTransfer) InterruptTransferDisclosures(at time.Time) {
	for i := range in.Facts.Operations {
		op := &in.Facts.Operations[i]
		if op.Kind != kindDisclosureOp || op.State != DisclosureProposed {
			continue
		}
		previous := op.State
		op.State, op.UpdatedAt = DisclosureInterrupted, at
		op.Revision++
		in.Facts.Events = append(in.Facts.Events, ledger.Event{OperationID: op.ID, Revision: op.Revision, Incarnation: op.Incarnation, From: previous, To: op.State, Actor: "project-transfer", At: at})
	}
}

func (s *Store) ExportProject(ctx context.Context, id string) (ProjectTransfer, error) {
	p, ok, err := s.Lookup(ctx, id)
	out := ProjectTransfer{Project: p, Facts: ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{}}}
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrUnknown
	}
	for _, kind := range []string{kindBinding, kindGrant, kindConfigGrant, kindDisclosure, originMappingKind, cloneKind} {
		raw, err := s.l.Bindings(ctx, kind)
		if err != nil {
			return out, err
		}
		selected := map[string]json.RawMessage{}
		for key, value := range raw {
			projectID := ""
			switch kind {
			case kindBinding:
				var b Binding
				if err := json.Unmarshal(value, &b); err != nil {
					return out, err
				}
				projectID = b.ProjectID
			case kindGrant, kindConfigGrant:
				var g Grant
				if err := json.Unmarshal(value, &g); err != nil {
					return out, err
				}
				projectID = g.Project
			case kindDisclosure, originMappingKind, cloneKind:
				var d struct {
					Project string `json:"project"`
				}
				if err := json.Unmarshal(value, &d); err != nil {
					return out, err
				}
				projectID = d.Project
			}
			if projectID == id {
				selected[key] = value
				if kind == kindBinding {
					out.Conversations = append(out.Conversations, key)
				}
			}
		}
		out.Facts.Bindings[kind] = selected
	}
	var ids []string
	for _, kind := range []string{kindDisclosureOp, cloneKind} {
		ops, err := s.l.Operations(ctx, kind, "")
		if err != nil {
			return out, err
		}
		for _, op := range ops {
			var d struct {
				Project string `json:"project"`
			}
			if err := json.Unmarshal(op.Data, &d); err != nil {
				return out, err
			}
			if d.Project == id {

				ids = append(ids, op.ID)
			}
		}
	}
	for _, conversation := range out.Conversations {
		if name, found, err := s.l.Name(ctx, bindingNameBase+conversation+"/project"); err != nil {
			return out, err
		} else if found {
			out.Facts.Names = append(out.Facts.Names, name)
		}
	}
	facts, err := s.l.ExportOperations(ctx, ids)
	out.Facts.Add(facts)
	return out, err
}

func (in *ProjectTransfer) Remap(m ledger.TransferIDs) error {
	for i := range in.Conversations {
		in.Conversations[i] = m.Conversation(in.Conversations[i])
	}
	values := in.Facts.Bindings[kindBinding]
	next := map[string]json.RawMessage{}
	for id, raw := range values {
		var b Binding
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		b.ConversationID = m.Conversation(b.ConversationID)
		encoded, _ := json.Marshal(b)
		next[m.Conversation(id)] = encoded
	}
	in.Facts.Bindings[kindBinding] = next
	for id, raw := range in.Facts.Bindings[kindDisclosure] {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if rawID, ok := value["task_id"]; ok {
			var id string
			if err := json.Unmarshal(rawID, &id); err != nil {
				return err
			}
			value["task_id"], _ = json.Marshal(m.Task(id))
		}
		in.Facts.Bindings[kindDisclosure][id], _ = json.Marshal(value)
	}
	for i := range in.Facts.Operations {
		op := &in.Facts.Operations[i]
		if op.Kind == kindDisclosureOp {
			var d DisclosureRequest
			if err := json.Unmarshal(op.Data, &d); err != nil {
				return err
			}
			d.TaskID = m.Task(d.TaskID)
			d.ConversationID = m.Conversation(d.ConversationID)
			op.Data, _ = json.Marshal(d)
		}
	}
	in.Facts.RemapEnvelopes(m)
	return nil
}

const originMappingKind = "project-origin-mapping"

type OriginMapping struct {
	Project    string             `json:"project"`
	HubID      string             `json:"hub_id"`
	TransferID string             `json:"transfer_id"`
	IDs        ledger.TransferIDs `json:"ids"`
}

func (s *Store) OriginMappings(ctx context.Context, id string) ([]OriginMapping, error) {
	raw, err := s.l.Bindings(ctx, originMappingKind)
	if err != nil {
		return nil, err
	}
	var out []OriginMapping
	for _, value := range raw {
		var m OriginMapping
		if err := json.Unmarshal(value, &m); err != nil {
			return nil, err
		}
		if m.Project == id {
			out = append(out, m)
		}
	}
	return out, nil
}
