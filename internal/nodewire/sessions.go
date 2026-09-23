package nodewire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/view"
)

// managedSessionPrefix marks the identity of a session a node opened and owns.
const managedSessionPrefix = "ns_"

// SessionOpenID is the stable native identity of one admitted open command.
func SessionOpenID(cluster, node, attempt, command, harness string) string {
	raw, _ := json.Marshal([]string{cluster, node, attempt, command, harness})
	hash := sha256.Sum256(raw)
	return managedSessionPrefix + hex.EncodeToString(hash[:])
}

// IsManagedSession reports whether id names a node-owned session: the node
// keeps its process and input receipts, and the hub only observes it.
func IsManagedSession(id string) bool { return strings.HasPrefix(id, managedSessionPrefix) }

const FeatureNativeHistory = "native_history.v1"

const FeatureNodeSessions = "node_sessions.v1"
const FeatureNativeResume = "native_resume.v1"
const StreamNodeSessions = "node_sessions"
const NodeSessionMaxBytes = 16 << 20

// SessionAuthority identifies the committed coordinator activation. It is a
// claim to validate against the node's authority service, never a bearer grant.
type SessionAuthority struct {
	ClusterID         string `json:"cluster_id"`
	CoordinatorNodeID string `json:"coordinator_node_id"`
	CoordinatorEpoch  uint64 `json:"coordinator_epoch"`
	WriterGeneration  uint64 `json:"writer_generation"`
}

// SessionBinding names the already admitted execution, independently of the
// coordinator process currently observing it.
type SessionBinding struct {
	NativeImportID  string `json:"native_import_id,omitempty"`
	PluginRuntimeID string `json:"plugin_runtime_id,omitempty"`
	ProjectID       string `json:"project_id"`
	SessionID       string `json:"session_id"`
	TaskID          string `json:"task_id"`
	AttemptID       string `json:"attempt_id"`
	NodeID          string `json:"node_id"`
	ExecutionEpoch  uint64 `json:"execution_epoch"`
	TaskEpoch       uint64 `json:"task_epoch"`
}

type SessionMedia struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"`
	URI  string `json:"uri,omitempty"`
}

// SessionAction names a node session operation without changing its JSON value.
type SessionAction string

const (
	SessionActionOpen         SessionAction = "open"
	SessionActionClose        SessionAction = "close"
	SessionActionPrompt       SessionAction = "prompt"
	SessionActionPoll         SessionAction = "poll"
	SessionActionAttach       SessionAction = "attach"
	SessionActionCapabilities SessionAction = "capabilities"
	SessionActionInspectOpen  SessionAction = "inspect-open"
	SessionActionCancelOpen   SessionAction = "cancel-open"
	SessionActionSettings     SessionAction = "settings"
	SessionActionAnswer       SessionAction = "answer"
	SessionActionOption       SessionAction = "option"
	SessionActionCancel       SessionAction = "cancel"
	SessionActionAbort        SessionAction = "abort"
	// Start is an authorization challenge inside open, not a client operation.
	SessionActionStart SessionAction = "start"
)

type SessionRequest struct {
	NativeImport  *nativehistory.Reference `json:"native_import,omitempty"`
	Plugin        *plugins.RuntimeRef      `json:"plugin,omitempty"`
	Action        SessionAction            `json:"action"`
	Authority     SessionAuthority         `json:"authority"`
	Binding       SessionBinding           `json:"binding"`
	ID            string                   `json:"id,omitempty"`
	Harness       string                   `json:"harness,omitempty"`
	Workdir       string                   `json:"workdir,omitempty"`
	MCPServers    []acp.MCPServer          `json:"mcp_servers,omitempty"`
	Permission    string                   `json:"permission,omitempty"`
	CommandID     string                   `json:"command_id,omitempty"`
	InputSequence uint64                   `json:"input_sequence,omitempty"`
	Text          string                   `json:"text,omitempty"`
	Media         []SessionMedia           `json:"media,omitempty"`
	After         uint64                   `json:"after,omitempty"`
	WaitMS        int                      `json:"wait_ms,omitempty"`
	QuestionID    string                   `json:"question_id,omitempty"`
	Answer        *SessionAnswer           `json:"answer,omitempty"`
	OptionID      string                   `json:"option_id,omitempty"`
	OptionValue   string                   `json:"option_value,omitempty"`

	// Only cold open may carry a proof; subsequent operations omit it.
	MCPAuthorizationRefresh *MCPAuthorizationRefresh `json:"mcp_authorization_refresh,omitempty"`
}

// MCPAuthorizationRefresh proves only the previous built-in steve HTTP MCP
// Authorization value. It is not execution authority or a reusable credential.
type MCPAuthorizationRefresh struct {
	PreviousAuthorization string `json:"previous_authorization"`
}

type SessionAnswer struct {
	CommandID string `json:"command_id"`
	Decision  string `json:"decision"`
	Choice    string `json:"choice,omitempty"`
	Text      string `json:"text,omitempty"`
}

type SessionQuestionState string

const (
	SessionQuestionPending     SessionQuestionState = "pending"
	SessionQuestionAnswered    SessionQuestionState = "answered"
	SessionQuestionInterrupted SessionQuestionState = "interrupted"
)

// Settled reports a question that can no longer change. An interrupted
// question belongs to a native process the node already saw stop, so no
// answer can still reach it.
func (s SessionQuestionState) Settled() bool {
	return s == SessionQuestionAnswered || s == SessionQuestionInterrupted
}

type SessionQuestion struct {
	ID         string               `json:"id"`
	CommandID  string               `json:"command_id"`
	Question   view.Question        `json:"question"`
	Permission *permission.Ask      `json:"permission,omitempty"`
	State      SessionQuestionState `json:"state"`
	Answer     *SessionAnswer       `json:"answer,omitempty"`
	CreatedAt  time.Time            `json:"created_at"`
}

type SessionCommand struct {
	// Receipt freezes terminal evidence before later rebind/process cleanup.
	Receipt         SessionReceipt      `json:"receipt,omitzero"`
	ID              string              `json:"id"`
	InputSequence   uint64              `json:"input_sequence"`
	State           SessionCommandState `json:"state"`
	CancelRequested bool                `json:"cancel_requested,omitempty"`
	// Missing means unknown. Only explicit not-dispatched proves that the
	// task prompt has not been submitted to the native client.
	DispatchState string   `json:"dispatch_state,omitempty"` // not-dispatched | dispatched
	Output        string   `json:"output,omitempty"`
	Activity      []string `json:"activity,omitempty"`
	Error         string   `json:"error,omitempty"`
	// Settled describes an answered ACP prompt. ProcessStopped is stronger:
	// the node observed exit of the original native agent process.
	Settled        bool `json:"settled"`
	ProcessStopped bool `json:"process_stopped"`
}

type SessionState struct {
	NativeImport  *nativehistory.Reference `json:"native_import,omitempty"`
	Plugin        *plugins.RuntimeRef      `json:"plugin,omitempty"`
	OpenReceipt   *SessionOpenReceipt      `json:"open_receipt,omitempty"`
	ID            string                   `json:"id"`
	ContextID     string                   `json:"context_id,omitempty"`
	Binding       SessionBinding           `json:"binding"`
	Harness       string                   `json:"harness"`
	State         SessionStatus            `json:"state"`
	Sequence      uint64                   `json:"sequence"`
	InputAccepted uint64                   `json:"input_accepted"`
	// NextInputSequence is a node hint for the requested, not-yet-accepted
	// command under Binding. It is neither execution authority nor an ack proof.
	NextInputSequence uint64            `json:"next_input_sequence,omitempty"`
	Settings          view.Settings     `json:"settings"`
	ModelOption       string            `json:"model_option,omitempty"`
	ModelChoices      []view.Choice     `json:"model_choices,omitempty"`
	SupportsHTTPMCP   bool              `json:"supports_http_mcp"`
	Progress          view.Progress     `json:"progress"`
	Command           *SessionCommand   `json:"command,omitempty"`
	Questions         []SessionQuestion `json:"questions"`
	ProcessStopped    bool              `json:"process_stopped"`
}

// SessionOpenReceipt binds a fresh inspect/cancel result to the original open.
// Only CancelledBeforeOpen proves that a missing open was durably fenced.
type SessionOpenReceipt struct {
	Action              SessionAction    `json:"action"`
	Authority           SessionAuthority `json:"authority"`
	CommandID           string           `json:"command_id"`
	CancelledBeforeOpen bool             `json:"cancelled_before_open,omitempty"`
}

type SessionReply struct {
	AuthorizeAction SessionAction `json:"authorize_action,omitempty"`
	State           *SessionState `json:"state,omitempty"`
	ErrorCode       string        `json:"error_code,omitempty"`
	Error           string        `json:"error,omitempty"`
	// NotStarted marks a refusal the node decided before it reserved
	// anything for the open: no durable record, no runtime, no process.
	NotStarted bool `json:"not_started,omitempty"`
}

// SessionAuthorization is a reply on the same authenticated RPC stream. It is
// never transferable to a different request or reusable as a bearer grant.
type SessionAuthorization struct {
	Allowed bool   `json:"allowed"`
	Error   string `json:"error,omitempty"`
}

// SessionNotDispatched reports failure before any session request bytes were
// sent. It makes no claim about an already existing native session.
type SessionNotDispatched struct{ Cause error }

func (e *SessionNotDispatched) Error() string { return e.Cause.Error() }
func (e *SessionNotDispatched) Unwrap() error { return e.Cause }

// SessionOpenNotStarted is the node's own answer that it refused an open
// before reserving anything for it. The node writes its durable record
// before any native process starts, so this refusal proves that no agent
// runs for that open and none ever will. The original writer needs no
// quarantine and the execution may be retried.
type SessionOpenNotStarted struct{ Cause error }

func (e *SessionOpenNotStarted) Error() string { return e.Cause.Error() }
func (e *SessionOpenNotStarted) Unwrap() error { return e.Cause }
