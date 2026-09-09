package state

import (
	"encoding/json"
	"sort"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plugins"
)

type PluginReference struct {
	Conversation string             `json:"conversation"`
	Agent        string             `json:"agent"`
	Archived     bool               `json:"archived"`
	Session      string             `json:"session"`
	Runtime      plugins.RuntimeRef `json:"runtime"`
}

// PluginReferences includes archived sessions because those can still be
// restored with their original native home and plugin version.
func PluginReferences(doc ledger.Doc) ([]PluginReference, error) {
	raw, found, err := doc.Load()
	if err != nil {
		return nil, err
	}
	if !found {
		return []PluginReference{}, nil
	}
	var state data
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	refs := []PluginReference{}
	for conversation, entry := range state.Conversations {
		for agent, session := range entry.Sessions {
			if session.PluginRuntime != nil {
				if err := session.PluginRuntime.Validate(); err != nil {
					return nil, err
				}
				refs = append(refs, PluginReference{Conversation: conversation, Agent: agent, Session: session.UpstreamID, Runtime: *session.PluginRuntime.Clone()})
			}
		}
		for _, session := range entry.Archived {
			if session.PluginRuntime != nil {
				if err := session.PluginRuntime.Validate(); err != nil {
					return nil, err
				}
				refs = append(refs, PluginReference{Conversation: conversation, Agent: session.AgentID, Session: session.UpstreamID, Archived: true, Runtime: *session.PluginRuntime.Clone()})
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Conversation+refs[i].Agent+refs[i].Runtime.ID < refs[j].Conversation+refs[j].Agent+refs[j].Runtime.ID
	})
	return refs, nil
}

func (s *Store) ForgetPluginRuntime(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	for name, conversation := range next.Conversations {
		for agent, session := range conversation.Sessions {
			if session.PluginRuntime != nil && session.PluginRuntime.ID == id {
				delete(conversation.Sessions, agent)
			}
		}
		archived := conversation.Archived[:0]
		for _, session := range conversation.Archived {
			if session.PluginRuntime == nil || session.PluginRuntime.ID != id {
				archived = append(archived, session)
			}
		}
		conversation.Archived = archived
		next.Conversations[name] = conversation
	}
	return s.replaceLocked(next)
}
