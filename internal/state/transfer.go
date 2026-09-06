package state

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/gopact-ai/steve/internal/ledger"
)

type ProjectTransfer struct {
	Project       string                  `json:"project"`
	Conversations map[string]Conversation `json:"conversations"`
}

func ExportProject(doc ledger.Doc, project string, ids []string) (ProjectTransfer, error) {
	out := ProjectTransfer{Project: project, Conversations: map[string]Conversation{}}
	raw, ok, err := doc.Load()
	if err != nil || !ok {
		return out, err
	}
	var d data
	if err := json.Unmarshal(raw, &d); err != nil {
		return out, err
	}
	for _, id := range ids {
		c, ok := d.Conversations[id]
		if !ok {
			continue
		}
		for key, s := range c.Sessions {
			if s.ProjectID != project {
				delete(c.Sessions, key)
				c.Preferences = nil
				continue
			}
			s.AgentToken = ""
			s.UpstreamID = ""
			s.CapabilityHash = ""
			s.InstructionsApplied = false
			s.Tainted = true
			c.Sessions[key] = s
		}
		archived := c.Archived[:0]
		for i := range c.Archived {
			if c.Archived[i].ProjectID != project {
				continue
			}
			c.Archived[i].AgentToken = ""
			c.Archived[i].UpstreamID = ""
			c.Archived[i].Tainted = true
			archived = append(archived, c.Archived[i])
		}
		c.Archived = archived
		out.Conversations[id] = c
	}
	return out, nil
}
func ImportProject(doc ledger.Doc, in ProjectTransfer) error {
	raw, ok, err := doc.Load()
	if err != nil {
		return err
	}
	d := data{Conversations: map[string]Conversation{}}
	if ok {
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
	}
	if d.Conversations == nil {
		d.Conversations = map[string]Conversation{}
	}
	for id, c := range in.Conversations {
		for _, a := range c.Archived {
			if a.ProjectID != in.Project || a.AgentToken != "" || a.UpstreamID != "" {
				return fmt.Errorf("invalid imported archived session")
			}
		}
		for _, s := range c.Sessions {
			if s.ProjectID != in.Project || s.AgentToken != "" || s.UpstreamID != "" {
				return fmt.Errorf("invalid imported session")
			}
		}
		if old, ok := d.Conversations[id]; ok && !reflect.DeepEqual(old, c) {
			return fmt.Errorf("conversation collision %s", id)
		}
		d.Conversations[id] = c
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	return doc.Save(raw)
}

func (in *ProjectTransfer) Remap(m ledger.TransferIDs) {
	all := map[string]Conversation{}
	for id, c := range in.Conversations {
		for key, s := range c.Sessions {
			s.ConversationID = m.Conversation(s.ConversationID)
			c.Sessions[key] = s
		}
		for i := range c.Archived {
			c.Archived[i].ConversationID = m.Conversation(c.Archived[i].ConversationID)
		}
		all[m.Conversation(id)] = c
	}
	in.Conversations = all
}
