package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type Session struct {
	ConversationID      string `json:"conversation_id"`
	AgentID             string `json:"agent_id"`
	HarnessID           string `json:"harness_id"`
	UpstreamID          string `json:"upstream_id,omitempty"`
	Workspace           string `json:"workspace"`
	CapabilityHash      string `json:"capability_hash"`
	InstructionsApplied bool   `json:"instructions_applied,omitempty"`
	Tainted             bool   `json:"tainted,omitempty"`
}

type Conversation struct {
	ActiveAgent string             `json:"active_agent"`
	Sessions    map[string]Session `json:"sessions"`
}

type data struct {
	Conversations map[string]Conversation `json:"conversations"`
}

type Store struct {
	path string
	mu   sync.Mutex
	data data
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, data: data{Conversations: map[string]Conversation{}}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s.data); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse state: expected one JSON object")
	}
	if s.data.Conversations == nil {
		s.data.Conversations = map[string]Conversation{}
	}
	return s, nil
}

func (s *Store) Conversation(id string) Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	conversation := s.data.Conversations[id]
	return cloneConversation(conversation)
}

func (s *Store) Check() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".state-check-*")
	if err != nil {
		return fmt.Errorf("check state directory: %w", err)
	}
	name := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close state check file: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove state check file: %w", err)
	}
	return nil
}

func (s *Store) SetActiveAgent(conversationID, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	conversation := next.Conversations[conversationID]
	conversation.ActiveAgent = agentID
	if conversation.Sessions == nil {
		conversation.Sessions = map[string]Session{}
	}
	next.Conversations[conversationID] = conversation
	return s.replaceLocked(next)
}

func (s *Store) SaveSession(session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	conversation := next.Conversations[session.ConversationID]
	if conversation.Sessions == nil {
		conversation.Sessions = map[string]Session{}
	}
	if existing, ok := conversation.Sessions[session.AgentID]; ok {
		if existing.HarnessID != session.HarnessID {
			return fmt.Errorf("session harness is immutable: %q != %q", existing.HarnessID, session.HarnessID)
		}
		if existing.Workspace != session.Workspace {
			return fmt.Errorf("session workspace is immutable: %q != %q", existing.Workspace, session.Workspace)
		}
	}
	conversation.Sessions[session.AgentID] = session
	next.Conversations[session.ConversationID] = conversation
	return s.replaceLocked(next)
}

func (s *Store) DeleteSession(conversationID, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	conversation := next.Conversations[conversationID]
	delete(conversation.Sessions, agentID)
	next.Conversations[conversationID] = conversation
	return s.replaceLocked(next)
}

func (s *Store) replaceLocked(next data) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return fmt.Errorf("create state file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure state file: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	s.data = next
	return nil
}

func cloneData(source data) data {
	clone := data{Conversations: make(map[string]Conversation, len(source.Conversations))}
	for id, conversation := range source.Conversations {
		clone.Conversations[id] = cloneConversation(conversation)
	}
	return clone
}

func cloneConversation(conversation Conversation) Conversation {
	clone := conversation
	clone.Sessions = make(map[string]Session, len(conversation.Sessions))
	for id, session := range conversation.Sessions {
		clone.Sessions[id] = session
	}
	return clone
}
