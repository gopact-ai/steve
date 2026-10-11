package turn

import (
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Source identifies the input in its adapter's namespace. A source is evidence
// of where work came from; it does not grant execution or queue authority.
type Source struct {
	Channel        string
	ConversationID string
	MessageID      string
	ChatType       protocol.ChatType
	Mentioned      bool
	// Origin marks work submitted on the user's behalf, such as a schedule
	// firing. It keeps unattended work separate from ordinary conversation work.
	Origin string
}

// Actor is the adapter-authenticated caller. ID remains native to Source.Channel:
// owner resolution and project permissions must not substitute another channel's
// identity. A neutral principal service can later resolve this pair explicitly.
type Actor struct {
	ID string
}

// ReplyContext holds adapter-owned delivery metadata. The current reply route
// remains the source address; ChatID and CardID are not task or queue authority.
// A completed turn does not itself prove that an external reply was delivered.
type ReplyContext struct {
	ChatID string
	CardID string
}

// Admission carries execution preconditions separately from source and reply
// metadata. Adapters supply these from their durable input; the coordinator
// still verifies them against the project, task and execution incarnation.
type Admission struct {
	// ExchangeID identifies the exact Console durable input. A channel message
	// identity alone must never acquire Console queue or completion authority.
	ExchangeID string
	// ExpectedProject fences an unattended submission to its creation-time project.
	ExpectedProject string
	// ExpectedTask binds a continuation to its original task.
	ExpectedTask    string
	ResumeAdmission task.ResumeAdmission
	// Queue waits for the running turn; parsed interrupt controls still determine
	// the final queue policy in Handle.
	Queue bool
}

// Request is the shared turn contract used by Console, Gateway and recovery.
// Transport adapters own authentication, durable acceptance and reply delivery;
// the coordinator owns execution admission. No request field proves delivery.
type Request struct {
	Source    Source
	Actor     Actor
	Reply     ReplyContext
	Admission Admission
	Locale    string
	Input     string
	Images    []harness.Media
	// Relocation is the original input and scoped history from its adapter.
	Relocation *RelocationContext

	OnProgress func(view.Progress)
	OnAskUser  acphost.AskUserFunc
	// OnTurnReady binds the durable input's observer to its task and attempt.
	OnTurnReady func(taskID, attemptID string)
	OnPhase     func(view.Phase)
	// OnStage reports preparation while the turn is still waking.
	OnStage func(view.Stage)
	OnAsk   permission.AskFunc
}

func (r Request) phase(p view.Phase) {
	if r.OnPhase != nil {
		r.OnPhase(p)
	}
}

func (r Request) stage(s view.Stage) {
	if r.OnStage != nil {
		r.OnStage(s)
	}
}

func (r Request) Address() channel.Address {
	return channel.Address{Channel: r.Source.Channel, Conversation: r.Source.ConversationID, Message: r.Source.MessageID}
}
