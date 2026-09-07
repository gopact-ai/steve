package nodewire

import (
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

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
	ProjectID      string `json:"project_id"`
	SessionID      string `json:"session_id"`
	TaskID         string `json:"task_id"`
	AttemptID      string `json:"attempt_id"`
	NodeID         string `json:"node_id"`
	ExecutionEpoch uint64 `json:"execution_epoch"`
	TaskEpoch      uint64 `json:"task_epoch"`
}

type SessionMedia struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"`
	URI  string `json:"uri,omitempty"`
}

type SessionRequest struct {
	Action        string           `json:"action"` // open | attach | prompt | poll | answer | settings | option | cancel | abort | close
	Authority     SessionAuthority `json:"authority"`
	Binding       SessionBinding   `json:"binding"`
	ID            string           `json:"id,omitempty"`
	Harness       string           `json:"harness,omitempty"`
	Workdir       string           `json:"workdir,omitempty"`
	MCPServers    []acp.MCPServer  `json:"mcp_servers,omitempty"`
	Permission    string           `json:"permission,omitempty"`
	CommandID     string           `json:"command_id,omitempty"`
	InputSequence uint64           `json:"input_sequence,omitempty"`
	Text          string           `json:"text,omitempty"`
	Media         []SessionMedia   `json:"media,omitempty"`
	After         uint64           `json:"after,omitempty"`
	WaitMS        int              `json:"wait_ms,omitempty"`
	QuestionID    string           `json:"question_id,omitempty"`
	Answer        *SessionAnswer   `json:"answer,omitempty"`
	OptionID      string           `json:"option_id,omitempty"`
	OptionValue   string           `json:"option_value,omitempty"`
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
	ID              string `json:"id"`
	InputSequence   uint64 `json:"input_sequence"`
	State           string `json:"state"` // accepted | running | completed | cancelled | uncertain
	CancelRequested bool   `json:"cancel_requested,omitempty"`
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
	ID              string            `json:"id"`
	Binding         SessionBinding    `json:"binding"`
	Harness         string            `json:"harness"`
	State           string            `json:"state"`
	Sequence        uint64            `json:"sequence"`
	InputAccepted   uint64            `json:"input_accepted"`
	Settings        view.Settings     `json:"settings"`
	ModelOption     string            `json:"model_option,omitempty"`
	ModelChoices    []view.Choice     `json:"model_choices,omitempty"`
	SupportsHTTPMCP bool              `json:"supports_http_mcp"`
	Progress        view.Progress     `json:"progress"`
	Command         *SessionCommand   `json:"command,omitempty"`
	Questions       []SessionQuestion `json:"questions"`
	ProcessStopped  bool              `json:"process_stopped"`
}

type SessionReply struct {
	State     *SessionState `json:"state,omitempty"`
	ErrorCode string        `json:"error_code,omitempty"`
	Error     string        `json:"error,omitempty"`
}
