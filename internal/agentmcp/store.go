package agentmcp

import (
	"context"
	"errors"
)

// Binding identifies one logical agent session. TaskID is empty for a chat;
// delegated and workflow sessions carry their task and delegation identity.
type Binding struct {
	ConversationID string `json:"conversation_id"`
	AgentID        string `json:"agent_id"`
	TaskID         string `json:"task_id,omitempty"`
	DelegatedBy    string `json:"delegated_by,omitempty"`
}

// GrantScope is the fixed execution authorized by a session's bearer token.
// A retained session supplies exactly the same scope after reconnecting.
type GrantScope struct {
	TaskID              string `json:"task_id"`
	TaskEpoch           uint64 `json:"task_epoch"`
	AttemptID           string `json:"attempt_id"`
	ExecutionGeneration uint64 `json:"execution_generation"`
	NodeID              string `json:"node_id"`
	SessionID           string `json:"session_id"`
}

var ErrGrantDenied = errors.New("agent MCP execution grant is not authorized")

// Store commits a grant's validation and mutation in the same authoritative
// transaction. The adapter owns task and attempt validation; no CLI credentials
// are stored here. Get/Put/Delete address only Steve MCP records.
type Store interface {
	Update(context.Context, func(StoreTx) error) error
}

type StoreTx interface {
	Get(kind, id string, value any) (bool, error)
	Put(kind, id string, value any) error
	Delete(kind, id string) error
	// Authorize checks the exact task, attempt, native session and execution
	// generation on every tools/call. It must not resolve a newer attempt.
	Authorize(Binding, GrantScope) error
	// Bind checks a first grant or explicit scope change. A change requires
	// the previous attempt to be settled and the same native chat session.
	Bind(Binding, *GrantScope, GrantScope) error
}
