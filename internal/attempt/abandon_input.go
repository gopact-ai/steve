package attempt

import (
	"strconv"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// AbandonInputTx identifies the immutable conversation input, never the task's
// latest anchor. Planning commands have their own round identity and therefore
// need the input captured when their task was created.
func AbandonInputTx(tx ledger.Reader, r Record) (string, error) {
	if r.Kind == KindChat {
		if r.TurnID == "" {
			return "", ErrAbandonInput
		}
		return r.TurnID, nil
	}
	if r.Kind != KindPlan {
		return "", nil
	}
	tracked, found, err := task.GetTx(tx, r.TaskID)
	if err != nil {
		return "", err
	}
	if !found || tracked.Origin != "plan" || tracked.PlanMessageID == "" || r.Execution == nil || r.Execution.TaskID != tracked.ID {
		return "", ErrAbandonInput
	}
	command, owned := strings.CutPrefix(r.TurnID, "plan/"+tracked.ID+"/r")
	revisionText, roundText, named := strings.Cut(command, "/prompt/")
	if !owned || !named {
		return "", ErrAbandonInput
	}
	revision, revErr := strconv.ParseUint(revisionText, 10, 64)
	round, roundErr := strconv.ParseUint(roundText, 10, 64)
	if revErr != nil || roundErr != nil || revision == 0 || round == 0 {
		return "", ErrAbandonInput
	}
	return tracked.PlanMessageID, nil
}
