package nodewire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/view"
)

// SessionOpenID is the stable native identity of one admitted open command.
func SessionOpenID(cluster, node, attempt, command, harness string) string {
	raw, _ := json.Marshal([]string{cluster, node, attempt, command, harness})
	hash := sha256.Sum256(raw)
	return "ns_" + hex.EncodeToString(hash[:])
}

const FeatureNodeSessions = "node_sessions.v1"
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
	Plugin        *plugins.RuntimeRef `json:"plugin,omitempty"`
	Action        SessionAction       `json:"action"`
	Authority     SessionAuthority    `json:"authority"`
	Binding       SessionBinding      `json:"binding"`
	ID            string              `json:"id,omitempty"`
	Harness       string              `json:"harness,omitempty"`
	Workdir       string              `json:"workdir,omitempty"`
	MCPServers    []acp.MCPServer     `json:"mcp_servers,omitempty"`
	Permission    string              `json:"permission,omitempty"`
	CommandID     string              `json:"command_id,omitempty"`
	InputSequence uint64              `json:"input_sequence,omitempty"`
	Text          string              `json:"text,omitempty"`
	Media         []SessionMedia      `json:"media,omitempty"`
	After         uint64              `json:"after,omitempty"`
	WaitMS        int                 `json:"wait_ms,omitempty"`
	QuestionID    string              `json:"question_id,omitempty"`
	Answer        *SessionAnswer      `json:"answer,omitempty"`
	OptionID      string              `json:"option_id,omitempty"`
	OptionValue   string              `json:"option_value,omitempty"`
}

type SessionAnswer struct {
	CommandID string `json:"command_id"`
	Decision  string `json:"decision"`
	Choice    string `json:"choice,omitempty"`
	Text      string `json:"text,omitempty"`
}

type SessionQuestion struct {
	ID         string          `json:"id"`
	CommandID  string          `json:"command_id"`
	Question   view.Question   `json:"question"`
	Permission *permission.Ask `json:"permission,omitempty"`
	State      string          `json:"state"`
	Answer     *SessionAnswer  `json:"answer,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

type SessionCommand struct {
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
	Plugin          *plugins.RuntimeRef `json:"plugin,omitempty"`
	OpenReceipt     *SessionOpenReceipt `json:"open_receipt,omitempty"`
	ID              string              `json:"id"`
	Binding         SessionBinding      `json:"binding"`
	Harness         string              `json:"harness"`
	State           SessionStatus       `json:"state"`
	Sequence        uint64              `json:"sequence"`
	InputAccepted   uint64              `json:"input_accepted"`
	Settings        view.Settings       `json:"settings"`
	ModelOption     string              `json:"model_option,omitempty"`
	ModelChoices    []view.Choice       `json:"model_choices,omitempty"`
	SupportsHTTPMCP bool                `json:"supports_http_mcp"`
	Progress        view.Progress       `json:"progress"`
	Command         *SessionCommand     `json:"command,omitempty"`
	Questions       []SessionQuestion   `json:"questions"`
	ProcessStopped  bool                `json:"process_stopped"`
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
