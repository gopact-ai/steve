package state

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plugins"
)

type Session struct {
	PluginSkillsFingerprint string              `json:"plugin_skills_fingerprint,omitempty"`
	PluginRuntime           *plugins.RuntimeRef `json:"plugin_runtime,omitempty"`
	ConversationID          string              `json:"conversation_id"`
	AgentID                 string              `json:"agent_id"`
	HarnessID               string              `json:"harness_id"`
	// NodeID is the machine the session's agent process runs on. Empty
	// means the hub itself. A restored session must reconnect to the same
	// node: the agent's workspace and its conversation live there.
	NodeID     string `json:"node_id,omitempty"`
	UpstreamID string `json:"upstream_id,omitempty"`
	Workspace  string `json:"workspace"`
	// ProjectID and ProjectVersion record the conversation's project
	// binding this session was opened under. A session is bound to one
	// binding: when the conversation moves to another project, or the same
	// project is re-bound, the session is stale and a new one is opened.
	ProjectID      string `json:"project_id,omitempty"`
	ProjectVersion int64  `json:"project_version,omitempty"`
	CapabilityHash string `json:"capability_hash"`
	// SessionConfigHash fences identity refreshes to unchanged session capabilities.
	SessionConfigHash   string `json:"session_config_hash,omitempty"`
	InstructionsApplied bool   `json:"instructions_applied,omitempty"`
	Tainted             bool   `json:"tainted,omitempty"`
	// AgentToken authenticates this session to the built-in messaging MCP
	// server. It is bound to this conversation+agent and feeds the session's
	// capability fingerprint, so it must survive restarts with the session.
	AgentToken string `json:"agent_token,omitempty"`
}

type Conversation struct {
	ActiveAgent string             `json:"active_agent"`
	Sessions    map[string]Session `json:"sessions"`
	// Archived holds sessions cleared out of Sessions, newest last. Clearing
	// a conversation ends the agent's context but must not put the history
	// out of reach: the agent session is closed, not deleted, so an archived
	// record is enough to load it again.
	Archived []Archived `json:"archived,omitempty"`
	// Preferences are what the owner chose for each agent in this
	// conversation — the model, a reasoning level, any selector the
	// harness exposes — by option id, "model" for the model. They outlive
	// sessions: a fresh session is opened with them.
	Preferences map[string]map[string]string `json:"preferences,omitempty"`
}

type Archived struct {
	Session
	ArchivedAt string `json:"archived_at"`
}

type PendingPair struct {
	OpenID    string `json:"open_id"`
	Code      string `json:"code"`
	CreatedAt string `json:"created_at"`
}

type pairingData struct {
	Pending  map[string]PendingPair `json:"pending,omitempty"`
	Approved []string               `json:"approved,omitempty"`
}

type data struct {
	Conversations map[string]Conversation `json:"conversations"`
	Pairing       pairingData             `json:"pairing,omitempty"`
	Onboarded     bool                    `json:"onboarded,omitempty"`
}

type Store struct {
	doc  ledger.Doc
	mu   sync.Mutex
	data data
}

// Open keeps the store in one JSON file; the gateway opens the ledger.
func Open(path string) (*Store, error) {
	return openWith(&ledger.FileDocument{Path: path})
}

// OpenLedger keeps the store in the ledger, importing a legacy file once.
func OpenLedger(l *ledger.Ledger, legacy string) (*Store, error) {
	doc := l.Document("state")
	if _, err := doc.Import(legacy); err != nil {
		return nil, err
	}
	return openWith(doc)
}

func openWith(doc ledger.Doc) (*Store, error) {
	s := &Store{doc: doc, data: data{Conversations: map[string]Conversation{}}}
	raw, ok, err := doc.Load()
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if !ok {
		return s, nil
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
	if err := s.doc.Check(); err != nil {
		return fmt.Errorf("state: %w", err)
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

// Preferences are the owner's choices for an agent in a conversation.
func (s *Store) Preferences(conversationID, agentID string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	if c, ok := s.data.Conversations[conversationID]; ok {
		for k, v := range c.Preferences[agentID] {
			out[k] = v
		}
	}
	return out
}

// SetPreferences merges a patch into an agent's preferences for a
// conversation; an empty value drops the key.
func (s *Store) SetPreferences(conversationID, agentID string, patch map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	conversation := next.Conversations[conversationID]
	if conversation.Sessions == nil {
		conversation.Sessions = map[string]Session{}
	}
	if conversation.Preferences == nil {
		conversation.Preferences = map[string]map[string]string{}
	}
	prefs := map[string]string{}
	for k, v := range conversation.Preferences[agentID] {
		prefs[k] = v
	}
	for k, v := range patch {
		if v == "" {
			delete(prefs, k)
		} else {
			prefs[k] = v
		}
	}
	if len(prefs) == 0 {
		delete(conversation.Preferences, agentID)
	} else {
		conversation.Preferences[agentID] = prefs
	}
	next.Conversations[conversationID] = conversation
	return s.replaceLocked(next)
}

func (s *Store) SaveSession(session Session) error {
	session.PluginRuntime = session.PluginRuntime.Clone()
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

func (s *Store) Allows(openID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshPairingLocked()
	for _, approved := range s.data.Pairing.Approved {
		if approved == openID {
			return true
		}
	}
	return false
}

func (s *Store) RequestPairing(openID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshPairingLocked()
	if openID == "" {
		return "", fmt.Errorf("pairing sender is required")
	}
	for _, approved := range s.data.Pairing.Approved {
		if approved == openID {
			return "", fmt.Errorf("sender already approved")
		}
	}
	if pending, ok := s.data.Pairing.Pending[openID]; ok && pending.Code != "" {
		return pending.Code, nil
	}
	code, err := newPairingCode()
	if err != nil {
		return "", err
	}
	next := cloneData(s.data)
	if next.Pairing.Pending == nil {
		next.Pairing.Pending = map[string]PendingPair{}
	}
	next.Pairing.Pending[openID] = PendingPair{
		OpenID: openID, Code: code, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.replaceLocked(next); err != nil {
		return "", err
	}
	return code, nil
}

func (s *Store) ApprovePairing(code string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshPairingLocked()
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return "", fmt.Errorf("pairing code is required")
	}
	var matched string
	for openID, pending := range s.data.Pairing.Pending {
		if subtle.ConstantTimeCompare([]byte(pending.Code), []byte(code)) == 1 {
			matched = openID
			break
		}
	}
	if matched == "" {
		return "", fmt.Errorf("unknown pairing code")
	}
	next := cloneData(s.data)
	delete(next.Pairing.Pending, matched)
	for _, approved := range next.Pairing.Approved {
		if approved == matched {
			return matched, s.replaceLocked(next)
		}
	}
	next.Pairing.Approved = append(next.Pairing.Approved, matched)
	if err := s.replaceLocked(next); err != nil {
		return "", err
	}
	return matched, nil
}

func (s *Store) PairingList() (pending []PendingPair, approved []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshPairingLocked()
	for _, item := range s.data.Pairing.Pending {
		pending = append(pending, item)
	}
	approved = append([]string(nil), s.data.Pairing.Approved...)
	return pending, approved
}

func (s *Store) ApprovedSenders() []string {
	_, approved := s.PairingList()
	return approved
}

// refreshPairingLocked re-reads pairing from the document: `steve pair`
// runs in another process and approves senders while the gateway is up.
func (s *Store) refreshPairingLocked() {
	raw, ok, err := s.doc.Load()
	if err != nil || !ok {
		return
	}
	var disk data
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&disk); err != nil {
		return
	}
	s.data.Pairing = pairingData{
		Pending:  make(map[string]PendingPair, len(disk.Pairing.Pending)),
		Approved: append([]string(nil), disk.Pairing.Approved...),
	}
	for id, pending := range disk.Pairing.Pending {
		s.data.Pairing.Pending[id] = pending
	}
}

func newPairingCode() (string, error) {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate pairing code: %w", err)
	}
	for i, b := range raw {
		raw[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(raw), nil
}

func (s *Store) Onboarded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Onboarded
}

func (s *Store) MarkOnboarded() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	next.Onboarded = true
	return s.replaceLocked(next)
}

// Relocate moves a conversation record to a new id. An existing destination
// is replaced so a newly bound home session wins.
func (s *Store) Relocate(from, to string) error {
	if from == "" || to == "" {
		return fmt.Errorf("state: relocate requires from and to")
	}
	if from == to {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	source, ok := next.Conversations[from]
	if !ok {
		return nil
	}
	for id, session := range source.Sessions {
		session.ConversationID = to
		source.Sessions[id] = session
	}
	next.Conversations[to] = source
	delete(next.Conversations, from)
	return s.replaceLocked(next)
}

var ErrNoArchive = errors.New("no such archived session")

// maxArchived bounds the per-conversation history so state.json cannot grow
// without limit. Oldest records are dropped first.
const maxArchived = 20

// ArchiveSession moves a session out of the active slot and into the
// conversation's history. It is what /clear uses: the agent forgets its
// context, but the record of how to reach it survives.
func (s *Store) ArchiveSession(conversationID, agentID, at string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	conversation := next.Conversations[conversationID]
	session, ok := conversation.Sessions[agentID]
	delete(conversation.Sessions, agentID)
	// A session the agent never gave an id to has nothing to go back to, so
	// archiving it would only add a row nobody can act on.
	if ok && session.UpstreamID != "" {
		conversation.Archived = append(conversation.Archived, Archived{Session: session, ArchivedAt: at})
		if len(conversation.Archived) > maxArchived {
			conversation.Archived = conversation.Archived[len(conversation.Archived)-maxArchived:]
		}
	}
	next.Conversations[conversationID] = conversation
	return s.replaceLocked(next)
}

// RestoreSession puts an archived session back in the active slot, newest
// first at index 1. The record is moved rather than copied, so the same
// session is never live and archived at once.
func (s *Store) RestoreSession(conversationID, agentID string, index int) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	conversation := next.Conversations[conversationID]
	matches := make([]int, 0, len(conversation.Archived))
	for i, archived := range conversation.Archived {
		if archived.AgentID == agentID {
			matches = append(matches, i)
		}
	}
	// Newest first, so index 1 is the session just cleared.
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}
	if index < 1 || index > len(matches) {
		return Session{}, ErrNoArchive
	}
	at := matches[index-1]
	restored := conversation.Archived[at].Session
	// Whatever is live now takes the restored one's place in the history,
	// so switching back and forth never loses either.
	if current, ok := conversation.Sessions[agentID]; ok && current.UpstreamID != "" {
		conversation.Archived[at] = Archived{Session: current, ArchivedAt: conversation.Archived[at].ArchivedAt}
	} else {
		conversation.Archived = append(conversation.Archived[:at], conversation.Archived[at+1:]...)
	}
	if conversation.Sessions == nil {
		conversation.Sessions = map[string]Session{}
	}
	// It has been away; the next turn must re-check drift before trusting it.
	restored.Tainted = false
	conversation.Sessions[agentID] = restored
	next.Conversations[conversationID] = conversation
	if err := s.replaceLocked(next); err != nil {
		return Session{}, err
	}
	return restored, nil
}

// ArchivedSessions lists one agent's history, newest first.
func (s *Store) ArchivedSessions(conversationID, agentID string) []Archived {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.data.Conversations[conversationID].Archived
	out := make([]Archived, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].AgentID == agentID {
			out = append(out, all[i])
		}
	}
	return out
}

// ClearTaint marks the session consistent again. Only the resume path may
// call it: resuming deliberately accepts a mid-flight session, because the
// agent replays its own on-disk history on load and the interrupted turn
// simply never got an answer.
func (s *Store) ClearTaint(conversationID, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	conversation := next.Conversations[conversationID]
	session, ok := conversation.Sessions[agentID]
	if !ok || !session.Tainted {
		return nil
	}
	session.Tainted = false
	conversation.Sessions[agentID] = session
	next.Conversations[conversationID] = conversation
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
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := s.doc.Save(raw); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	s.data = next
	return nil
}

func cloneData(source data) data {
	clone := data{
		Conversations: make(map[string]Conversation, len(source.Conversations)),
		Pairing: pairingData{
			Pending:  make(map[string]PendingPair, len(source.Pairing.Pending)),
			Approved: append([]string(nil), source.Pairing.Approved...),
		},
		Onboarded: source.Onboarded,
	}
	for id, conversation := range source.Conversations {
		clone.Conversations[id] = cloneConversation(conversation)
	}
	for id, pending := range source.Pairing.Pending {
		clone.Pairing.Pending[id] = pending
	}
	return clone
}

func cloneConversation(conversation Conversation) Conversation {
	clone := conversation
	clone.Archived = append([]Archived(nil), conversation.Archived...)
	for i := range clone.Archived {
		clone.Archived[i].PluginRuntime = clone.Archived[i].PluginRuntime.Clone()
	}
	if conversation.Preferences != nil {
		clone.Preferences = make(map[string]map[string]string, len(conversation.Preferences))
		for agent, prefs := range conversation.Preferences {
			copied := make(map[string]string, len(prefs))
			for k, v := range prefs {
				copied[k] = v
			}
			clone.Preferences[agent] = copied
		}
	}
	clone.Sessions = make(map[string]Session, len(conversation.Sessions))
	for id, session := range conversation.Sessions {
		session.PluginRuntime = session.PluginRuntime.Clone()
		clone.Sessions[id] = session
	}
	return clone
}
