package harness

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// admitReadyTerminals drains only current original-input intents. The node
// owns once-only payload consumption, so a lost reply cannot start work twice.
func (s *managedSession) admitReadyTerminals(ctx context.Context, request nodewire.SessionRequest, state nodewire.SessionState) error {
	if len(state.PendingTerminalStarts) == 0 {
		return nil
	}
	if !state.TerminalAdmission || len(state.PendingTerminalStarts) > 32 || state.Binding != request.Binding || state.Command == nil || state.Command.ID != request.CommandID || !state.Command.State.Active() ||
		(request.InputSequence != 0 && request.InputSequence != state.Command.InputSequence) {
		return errors.New("terminal admission projection differs from original input")
	}
	seen := map[string]bool{}
	for _, start := range state.PendingTerminalStarts {
		if len(start.ID) != 67 || !strings.HasPrefix(start.ID, "nt_") || seen[start.ID] || start.CommandID != request.CommandID || start.InputSequence == 0 || start.InputSequence != state.Command.InputSequence || start.Generation == 0 {
			return errors.New("terminal admission intent identity differs")
		}
		if _, err := hex.DecodeString(start.ID[3:]); err != nil {
			return errors.New("terminal admission identity is malformed")
		}
		seen[start.ID] = true
	}
	// Validate the whole bounded projection before consuming any gate.
	for _, start := range state.PendingTerminalStarts {
		admit := request
		admit.Action = nodewire.SessionActionTerminalAdmit
		admit.InputSequence = state.Command.InputSequence
		admit.Text = ""
		admit.Media = nil
		admit.Answer = nil
		admit.MCPAuthorizationRefresh = nil
		copy := start
		admit.TerminalStart = &copy
		if _, err := s.call(ctx, admit); err != nil {
			var classified interface{ SessionErrorCode() string }
			if errors.As(err, &classified) && classified.SessionErrorCode() == "busy" {
				// This exact refusal precedes consumption. Keep observing and
				// obtain new authority on a later poll; never retry an unknown
				// reply or reuse the last challenge as a cached permit.
				continue
			}
			return err
		}
	}
	return nil
}
