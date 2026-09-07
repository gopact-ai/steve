package agentmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/channel"
)

type grant struct {
	Binding Binding     `json:"binding"`
	Scope   *GrantScope `json:"scope,omitempty"`
	Revoked bool        `json:"revoked,omitempty"`
}
type grantIndex struct {
	Digest string `json:"digest"`
}
type grantContext struct {
	Digest string
	Grant  grant
}
type grantContextKey struct{}

// ScopeFromContext carries the already authorized fixed execution to platform
// tool consumers. They must not replace it with a task's newest attempt.
func ScopeFromContext(ctx context.Context) (GrantScope, bool) {
	fixed, ok := ctx.Value(grantContextKey{}).(grantContext)
	if !ok || fixed.Grant.Scope == nil {
		return GrantScope{}, false
	}
	return *fixed.Grant.Scope, true
}

// AuthorizeContext checks a tool's original grant again inside a consumer's
// write transaction. Non-MCP callers retain their own authorization policy.
func AuthorizeContext(ctx context.Context, tx StoreTx) error {
	fixed, ok := ctx.Value(grantContextKey{}).(grantContext)
	if !ok {
		return nil
	}
	if fixed.Grant.Scope == nil {
		return ErrGrantDenied
	}
	current, err := loadGrant(tx, fixed.Digest)
	if err != nil {
		return err
	}
	if current.Scope == nil || current.Binding != fixed.Grant.Binding || *current.Scope != *fixed.Grant.Scope {
		return ErrGrantDenied
	}
	return tx.Authorize(current.Binding, *current.Scope)
}

type conversationState struct {
	Address channel.Address `json:"address"`
	Epoch   uint64          `json:"epoch"`
	Style   string          `json:"style,omitempty"`
	Interim bool            `json:"interim,omitempty"`
}
type messageState struct {
	Epoch    uint64                   `json:"epoch"`
	Count    int                      `json:"count"`
	Updates  int                      `json:"updates"`
	Messages map[string]messageRecord `json:"messages"`
}
type messageRecord struct {
	Address         channel.Address `json:"address"`
	Attribution     string          `json:"attribution,omitempty"`
	Busy            bool            `json:"busy,omitempty"`
	Version         uint64          `json:"version"`
	Format          string          `json:"format"`
	Sequence        int             `json:"sequence"`
	Progress        string          `json:"progress,omitempty"`
	IntentID        string          `json:"intent_id,omitempty"`
	PendingTool     string          `json:"pending_tool,omitempty"`
	PendingProgress string          `json:"pending_progress,omitempty"`
}

func publicBinding(b binding) Binding {
	return Binding{b.conversationID, b.agentID, b.taskID, b.delegatedBy}
}
func privateBinding(b Binding) binding {
	return binding{b.ConversationID, b.AgentID, b.TaskID, b.DelegatedBy}
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func bindingKey(b Binding) string { raw, _ := json.Marshal(b); return digest(string(raw)) }

// SetStore attaches an activation's authoritative store before capabilities
// are registered. Any failed persistence fences this gate immediately and
// invokes onFailure once; callers never continue with an in-memory grant.
func (s *Server) SetStore(store Store, onFailure func(error)) error {
	if store == nil {
		return errors.New("agentmcp: store is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil || len(s.tokens) != 0 || len(s.anchors) != 0 {
		return errors.New("agentmcp: store must be configured before use")
	}
	s.store = store
	s.onFailure = onFailure
	return nil
}

func (s *Server) failLocked(err error) error {
	if err == nil {
		return nil
	}
	if s.storeErr == nil {
		s.storeErr = fmt.Errorf("agentmcp: durable state unavailable: %w", err)
		if s.onFailure != nil {
			go s.onFailure(s.storeErr)
		}
	}
	return s.storeErr
}

func loadGrant(tx StoreTx, hash string) (grant, error) {
	var g grant
	ok, err := tx.Get("grant", hash, &g)
	if err != nil {
		return g, err
	}
	if !ok || g.Revoked {
		return g, ErrGrantDenied
	}
	var index grantIndex
	ok, err = tx.Get("binding", bindingKey(g.Binding), &index)
	if err != nil {
		return g, err
	}
	if !ok || index.Digest != hash {
		return g, ErrGrantDenied
	}
	return g, nil
}

func (s *Server) prepareLocked(b binding, token string) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	if s.store != nil {
		hash := digest(token)
		binding := publicBinding(b)
		err := s.store.Update(context.Background(), func(tx StoreTx) error {
			var existing grant
			found, err := tx.Get("grant", hash, &existing)
			if err != nil {
				return err
			}
			if found && (existing.Revoked || existing.Binding != binding) {
				return ErrGrantDenied
			}
			var index grantIndex
			foundIndex, err := tx.Get("binding", bindingKey(binding), &index)
			if err != nil {
				return err
			}
			if foundIndex && index.Digest == hash && found {
				return nil
			}
			if found {
				return ErrGrantDenied
			}
			if foundIndex {
				var old grant
				ok, err := tx.Get("grant", index.Digest, &old)
				if err != nil {
					return err
				}
				if ok {
					old.Revoked = true
					if err := tx.Put("grant", index.Digest, old); err != nil {
						return err
					}
				}
			}
			if err := tx.Put("grant", hash, grant{Binding: binding}); err != nil {
				return err
			}
			return tx.Put("binding", bindingKey(binding), grantIndex{hash})
		})
		if err != nil {
			return s.failLocked(err)
		}
	}
	if old, ok := s.byBind[b]; ok && old != token {
		delete(s.tokens, old)
	}
	s.tokens[token] = b
	s.byBind[b] = token
	return nil
}

// BindExecution seals a prepared bearer token to one admitted native session.
// Retained replay is idempotent. The store permits a new scope only for an
// explicit next chat turn after the prior execution settled in this session.
func (s *Server) BindExecution(ctx context.Context, b Binding, scope GrantScope) error {
	if b.ConversationID == "" || b.AgentID == "" || scope.TaskID == "" || scope.AttemptID == "" || scope.NodeID == "" || scope.ExecutionGeneration == 0 || !strings.HasPrefix(scope.SessionID, "ns_") || (b.TaskID != "" && b.TaskID != scope.TaskID) {
		return ErrGrantDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.storeErr != nil {
		return s.storeErr
	}
	if s.store == nil {
		return nil
	}
	err := s.store.Update(ctx, func(tx StoreTx) error {
		var index grantIndex
		ok, err := tx.Get("binding", bindingKey(b), &index)
		if err != nil {
			return err
		}
		if !ok {
			return ErrGrantDenied
		}
		g, err := loadGrant(tx, index.Digest)
		if err != nil {
			return err
		}
		if g.Binding != b {
			return ErrGrantDenied
		}
		if err := tx.Bind(b, g.Scope, scope); err != nil {
			return err
		}
		if g.Scope != nil && *g.Scope == scope {
			return nil
		}
		g.Scope = &scope
		return tx.Put("grant", index.Digest, g)
	})
	if err != nil && !errors.Is(err, ErrGrantDenied) {
		return s.failLocked(err)
	}
	return err
}

func (s *Server) authenticate(ctx context.Context, token string, tools bool) (binding, context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.storeErr != nil {
		return binding{}, ctx, s.storeErr
	}
	if s.store == nil {
		b, ok := s.tokens[token]
		if token == "" || !ok {
			return b, ctx, ErrGrantDenied
		}
		return b, ctx, nil
	}
	var g grant
	hash := digest(token)
	err := s.store.Update(ctx, func(tx StoreTx) error {
		var err error
		g, err = loadGrant(tx, hash)
		if err != nil {
			return err
		}
		if tools {
			if g.Scope == nil {
				return ErrGrantDenied
			}
			return tx.Authorize(g.Binding, *g.Scope)
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, ErrGrantDenied) && ctx.Err() == nil {
			err = s.failLocked(err)
		}
		return binding{}, ctx, err
	}
	return privateBinding(g.Binding), context.WithValue(ctx, grantContextKey{}, grantContext{hash, g}), nil
}

func (s *Server) revokeLocked(token string) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	if s.store != nil {
		hash := digest(token)
		err := s.store.Update(context.Background(), func(tx StoreTx) error {
			var g grant
			ok, err := tx.Get("grant", hash, &g)
			if err != nil {
				return err
			}
			if !ok || g.Revoked {
				return nil
			}
			g.Revoked = true
			if err := tx.Put("grant", hash, g); err != nil {
				return err
			}
			var index grantIndex
			ok, err = tx.Get("binding", bindingKey(g.Binding), &index)
			if err != nil {
				return err
			}
			if ok && index.Digest == hash {
				return tx.Delete("binding", bindingKey(g.Binding))
			}
			return nil
		})
		if err != nil {
			return s.failLocked(err)
		}
	}
	if b, ok := s.tokens[token]; ok {
		if s.byBind[b] == token {
			delete(s.byBind, b)
		}
		delete(s.tokens, token)
	}
	return nil
}

func (s *Server) loadConversationLocked(ctx context.Context, conversation string, b *binding) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	if s.store == nil {
		return nil
	}
	loadConversation := !s.loadedConversations[conversation]
	loadMessages := b != nil && !s.loadedMessages[*b]
	if !loadConversation && !loadMessages {
		return nil
	}
	var state conversationState
	var messages messageState
	var present, sent bool
	err := s.store.Update(ctx, func(tx StoreTx) error {
		var err error
		if loadConversation {
			present, err = tx.Get("conversation", conversation, &state)
			if err != nil {
				return err
			}
		}
		if loadMessages {
			sent, err = tx.Get("messages", bindingKey(publicBinding(*b)), &messages)
		}
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return s.failLocked(err)
	}
	if loadConversation {
		s.loadedConversations[conversation] = true
	}
	if loadMessages {
		s.loadedMessages[*b] = true
	}
	if present {
		s.anchors[conversation] = &anchor{address: state.Address, epoch: state.Epoch}
		s.styles[conversation] = state.Style
		s.interims[conversation] = state.Interim
	}
	if sent {
		st := &sentState{epoch: messages.Epoch, count: messages.Count, updates: messages.Updates, ids: map[string]sentMsg{}}
		for id, r := range messages.Messages {
			st.ids[id] = sentMsg{address: r.Address, attribution: r.Attribution, busy: r.Busy, version: r.Version, format: r.Format, seq: r.Sequence, progress: r.Progress, intentID: r.IntentID, pendingTool: r.PendingTool, pendingProgress: r.PendingProgress}
		}
		s.sent[*b] = st
	}
	return nil
}

func (s *Server) saveConversationLocked(ctx context.Context, conversation string) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	if s.store == nil {
		return nil
	}
	state := s.conversationLocked(conversation)
	return s.failLocked(s.store.Update(ctx, func(tx StoreTx) error { return tx.Put("conversation", conversation, state) }))
}
func (s *Server) conversationLocked(conversation string) conversationState {
	state := conversationState{Style: s.styles[conversation], Interim: s.interims[conversation]}
	if a := s.anchors[conversation]; a != nil {
		state.Address = a.address
		state.Epoch = a.epoch
	}
	return state
}
func (s *Server) saveMessagesLocked(ctx context.Context, b binding, authorize bool) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	if s.store == nil {
		return nil
	}
	st := s.sent[b]
	state := messageState{Epoch: st.epoch, Count: st.count, Updates: st.updates, Messages: map[string]messageRecord{}}
	for id, r := range st.ids {
		state.Messages[id] = messageRecord{Address: r.address, Attribution: r.attribution, Busy: r.busy, Version: r.version, Format: r.format, Sequence: r.seq, Progress: r.progress, IntentID: r.intentID, PendingTool: r.pendingTool, PendingProgress: r.pendingProgress}
	}
	conversation := s.conversationLocked(b.conversationID)
	if st.count > 0 && st.epoch == conversation.Epoch {
		conversation.Interim = true
	}
	err := s.store.Update(ctx, func(tx StoreTx) error {
		if authorize {
			if _, ok := ScopeFromContext(ctx); !ok {
				return ErrGrantDenied
			}
			if err := AuthorizeContext(ctx, tx); err != nil {
				return err
			}
		}
		if err := tx.Put("messages", bindingKey(publicBinding(b)), state); err != nil {
			return err
		}
		return tx.Put("conversation", b.conversationID, conversation)
	})
	if err != nil && !errors.Is(err, ErrGrantDenied) {
		return s.failLocked(err)
	}
	if err == nil {
		s.interims[b.conversationID] = conversation.Interim
	}
	return err
}
