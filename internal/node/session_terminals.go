package node

import (
	"encoding/hex"
	"errors"
	"github.com/gopact-ai/steve/internal/nodewire"
	"strings"
)

// sessionTerminal stores cleanup ownership only, never command environment,
// credentials, payload, output or a reusable execution grant.
type sessionTerminal struct {
	ID             string                    `json:"id"`
	UpstreamID     string                    `json:"upstream_id"`
	Generation     uint64                    `json:"generation"`
	CommandID      string                    `json:"command_id"`
	InputSequence  uint64                    `json:"input_sequence"`
	Binding        nodewire.SessionBinding   `json:"binding"`
	Authority      nodewire.SessionAuthority `json:"authority"`
	Phase          string                    `json:"phase"`
	PayloadStarted bool                      `json:"payload_started,omitempty"`
	Preparation    terminalPreparation       `json:"preparation,omitzero"`
	Process        sessionProcess            `json:"process,omitzero"`
}

// terminalPreparation names a durable, inert transition: the child may
// still be in the original Agent group or lead its reserved new group.
// It is not an active group Capture and never permits payload execution.
type terminalPreparation struct {
	PID         int    `json:"pid"`
	Start       uint64 `json:"start"`
	ParentGroup int    `json:"parent_group"`
	Mark        string `json:"mark"`
}

func validTerminalIdentity(id string) bool {
	if len(id) != 67 || !strings.HasPrefix(id, "nt_") {
		return false
	}
	_, err := hex.DecodeString(id[3:])
	return err == nil
}

func validTerminalProcess(process sessionProcess) bool {
	id := process.Identity
	return id.Group > 0 && id.Leader == id.Group && id.Start > 0 && id.Mark != "" && len(id.Mark) <= 128 && process.Boot != "" && len(process.Boot) <= 128 && len(process.Namespace) <= 256 && len(process.Machine) <= 128
}

func validateSessionTerminals(record sessionRecord) error {
	if record.State.TerminalAdmission && record.Format != 2 {
		return errors.New("terminal negotiation requires terminal-aware cleanup format")
	}
	if record.Format == 1 {
		if len(record.Terminals) != 0 {
			return errors.New("legacy session format cannot retain terminal ownership")
		}
		return nil
	}
	if record.Format != 2 || len(record.Terminals) > 32 {
		return errors.New("unsupported or unbounded terminal ownership record")
	}
	for key, owner := range record.Terminals {
		if !validTerminalProcess(record.Process) || owner.InputSequence > record.State.InputAccepted {
			return errors.New("terminal lacks its original Agent/input cleanup owner")
		}
		if !validTerminalIdentity(key) || owner.ID != key || owner.UpstreamID == "" || len(owner.UpstreamID) > 256 || owner.Generation == 0 || !sessionNameValid(owner.CommandID) || owner.InputSequence == 0 || owner.Binding.NodeID != record.State.Binding.NodeID || owner.Binding.ProjectID != record.State.Binding.ProjectID || owner.Binding.ExecutionEpoch == 0 || owner.Authority.ClusterID != record.ClusterID || owner.Authority.CoordinatorEpoch == 0 || owner.Authority.WriterGeneration == 0 {
			return errors.New("terminal ownership identity differs from its original session")
		}
		if owner.Phase != "stopped" && (owner.UpstreamID != record.UpstreamID || owner.Generation != record.Generation || owner.Binding != record.State.Binding) {
			return errors.New("live terminal differs from its original native context")
		}
		if owner.PayloadStarted && (owner.Phase == "reserved" || owner.Phase == "preparing") {
			return errors.New("terminal payload predates durable active ownership")
		}
		switch owner.Phase {
		case "reserved":
			if owner.Preparation != (terminalPreparation{}) || owner.Process != (sessionProcess{}) {
				return errors.New("reserved terminal falsely claims process ownership")
			}
		case "preparing":
			prep := owner.Preparation
			if prep.PID <= 0 || prep.PID == record.Process.Leader || prep.Start < record.Process.Start || prep.ParentGroup != record.Process.Group || prep.Mark == "" || len(prep.Mark) > 128 || owner.Process != (sessionProcess{}) {
				return errors.New("terminal transition ownership is incomplete")
			}
		case "active", "stopping":
			if !validTerminalProcess(owner.Process) || owner.Process.Group == record.Process.Group || owner.Process.Start < record.Process.Start || owner.Process.Place != record.Process.Place {
				return errors.New("terminal group ownership is incomplete")
			}
			if owner.Preparation != (terminalPreparation{}) {
				return errors.New("active terminal retained ambiguous group transition")
			}
		case "stopped":
			// No process is inferred from zero fields. The explicit stopped phase
			// is recorded only after the responsible owner proves cleanup.
		default:
			return errors.New("unknown terminal ownership phase")
		}
		if owner.Phase != "stopped" && record.State.ProcessStopped {
			return errors.New("native stop omitted outstanding terminal ownership")
		}
		if owner.Phase != "stopped" {
			for _, command := range record.Commands {
				if command.ProcessStopped {
					return errors.New("command stop omitted outstanding terminal ownership")
				}
			}
		}
	}
	return nil
}

func sessionTerminalsStopped(record sessionRecord) bool {
	for _, owner := range record.Terminals {
		if owner.Phase != "stopped" {
			return false
		}
	}
	return true
}

func validateTerminalTransition(before, next sessionRecord) error {
	if err := validateSessionTerminals(next); err != nil {
		return err
	}
	if before.Format == 2 && next.Format != 2 {
		return errors.New("terminal-aware session format cannot be downgraded")
	}
	for id, old := range before.Terminals {
		current, exists := next.Terminals[id]
		if !exists {
			if old.Phase != "stopped" {
				return errors.New("unstopped terminal ownership cannot be forgotten")
			}
			continue
		}
		if old.ID != current.ID || old.UpstreamID != current.UpstreamID || old.Generation != current.Generation || old.CommandID != current.CommandID || old.InputSequence != current.InputSequence || old.Binding != current.Binding || old.Authority != current.Authority {
			return errors.New("original terminal ownership tuple cannot change")
		}
		if old.PayloadStarted && !current.PayloadStarted {
			return errors.New("consumed terminal payload cannot be replayed")
		}
		if !old.PayloadStarted && current.PayloadStarted && (old.Phase != "active" || current.Phase != "active") {
			return errors.New("terminal payload starts only from original active ownership")
		}
		if old.Phase == current.Phase {
			if old.Preparation != current.Preparation || old.Process != current.Process {
				return errors.New("terminal cleanup facts changed without a phase transition")
			}
			continue
		}
		allowed := false
		switch old.Phase {
		case "reserved":
			allowed = current.Phase == "preparing" || current.Phase == "stopped"
		case "preparing":
			allowed = current.Phase == "active" || current.Phase == "stopped"
			if current.Phase == "active" && (current.Process.Group != old.Preparation.PID || current.Process.Start != old.Preparation.Start || current.Process.Mark != old.Preparation.Mark) {
				return errors.New("terminal group split differs from its committed preparation")
			}
		case "active":
			allowed = current.Phase == "stopping" || current.Phase == "stopped"
		case "stopping":
			allowed = current.Phase == "stopped"
		}
		if !allowed {
			return errors.New("terminal ownership phase cannot be reopened or skipped")
		}
		if current.Phase == "stopped" && (current.Preparation != old.Preparation || current.Process != old.Process) {
			return errors.New("terminal stop must preserve its original cleanup identity")
		}
		if old.Phase == "active" && current.Phase == "stopping" && old.Process != current.Process {
			return errors.New("terminal stopping replaced its original group")
		}
	}
	for id, owner := range next.Terminals {
		if _, exists := before.Terminals[id]; !exists && before.State.Sequence != 0 && owner.Phase != "reserved" {
			return errors.New("new terminal ownership requires an original reservation")
		}
	}
	return nil
}

func validateTerminalCommandStop(record sessionRecord) error {
	if !sessionTerminalsStopped(record) {
		for _, command := range record.Commands {
			if command.ProcessStopped {
				return errors.New("command stop omitted outstanding terminal ownership")
			}
		}
	}
	return nil
}
