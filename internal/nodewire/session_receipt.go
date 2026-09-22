package nodewire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/view"
)

// SessionReceipt identifies one immutable, terminal native input. It is not a
// bearer grant: deletion requires committed hub result/accounting/delivery
// verification of this exact original identity.
type SessionReceipt struct {
	Version       int            `json:"version"`
	SessionID     string         `json:"session_id"`
	ContextID     string         `json:"context_id"`
	Binding       SessionBinding `json:"binding"`
	CommandID     string         `json:"command_id"`
	InputSequence uint64         `json:"input_sequence"`
	Digest        string         `json:"digest"`
}

const StreamNodeReceipts = "node_receipts"
const FeatureNodeReceipts = "node_receipts.v1"

// A receipt acknowledgement is deliberately separate from session actions:
// execution authority alone cannot delete original result evidence.
type SessionReceiptRequest struct {
	Authority SessionAuthority `json:"authority"`
	Receipt   SessionReceipt   `json:"receipt"`
}

type SessionReceiptReply struct {
	Authorize *SessionReceipt `json:"authorize,omitempty"`
	Ack       *SessionReceipt `json:"ack,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func (r SessionReceipt) Validate() error {
	if r.Version != 1 || r.SessionID == "" || r.ContextID == "" || r.CommandID == "" || r.InputSequence == 0 ||
		r.Binding.NodeID == "" || r.Binding.AttemptID == "" || r.Binding.TaskID == "" || r.Binding.SessionID == "" ||
		r.Binding.ExecutionEpoch == 0 || r.Binding.TaskEpoch == 0 {
		return errors.New("node receipt lacks its original execution identity")
	}
	digest, err := hex.DecodeString(r.Digest)
	if err != nil || len(digest) != sha256.Size || r.Digest != strings.ToLower(r.Digest) {
		return errors.New("node receipt has an invalid terminal digest")
	}
	return nil
}

// NewSessionReceipt snapshots terminal evidence at the node's durable
// transition. Callers must authenticate its source; computing a digest alone
// proves neither execution nor delivery.
func NewSessionReceipt(state SessionState) (SessionReceipt, error) {
	if state.Command == nil || !state.Command.Settled ||
		state.Command.State != SessionCommandCompleted && state.Command.State != SessionCommandCancelled {
		return SessionReceipt{}, errors.New("node receipt requires a settled terminal command")
	}
	receipt := SessionReceipt{Version: 1, SessionID: state.ID, ContextID: state.ContextID, Binding: state.Binding,
		CommandID: state.Command.ID, InputSequence: state.Command.InputSequence}
	questions := slices.Clone(state.Questions)
	slices.SortFunc(questions, func(a, b SessionQuestion) int { return strings.Compare(a.ID, b.ID) })
	for i, question := range questions {
		if question.CommandID != receipt.CommandID || question.ID == "" || !question.State.Settled() ||
			i > 0 && questions[i-1].ID == question.ID {
			return SessionReceipt{}, errors.New("node receipt has pending or inconsistent question evidence")
		}
	}
	command := *state.Command
	// These flags may change during later process cleanup, unlike the prompt's
	// output, spend and question outcomes frozen at this transition.
	command.ProcessStopped, command.CancelRequested = false, false
	command.Receipt = SessionReceipt{}
	payload := struct {
		Receipt   SessionReceipt
		Command   SessionCommand
		Progress  view.Progress
		Questions []SessionQuestion
	}{receipt, command, state.Progress, questions}
	raw, err := json.Marshal(payload)
	if err != nil {
		return SessionReceipt{}, err
	}
	digest := sha256.Sum256(raw)
	receipt.Digest = hex.EncodeToString(digest[:])
	if err := receipt.Validate(); err != nil {
		return SessionReceipt{}, err
	}
	return receipt, nil
}
