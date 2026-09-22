package delegate

import (
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/task"
)

func recoveredUsage(record attempt.Record) task.RecoveryUsage {
	u := record.Usage
	if u == nil {
		return task.RecoveryUsage{}
	}
	return task.RecoveryUsage{
		Tokens: task.FromUsage(uint64(max(u.Input, 0)), uint64(max(u.Output, 0)),
			uint64(max(u.CachedRead, 0)), uint64(max(u.CachedWrite, 0))),
		Model: u.Model, Reported: u.Reported,
	}
}

// A terminal result does not itself join an old observer or settle its usage.
// Retry this projection even for finished children: a native stop handler may
// finish after task/result persistence. Registry checks the original token and
// every owner/handler join, never a replacement execution's current epoch.
func (s *Service) resolveRecovered(record attempt.Record, tracked task.Task) {
	if s.executions == nil || record.Kind != attempt.KindDelegate || record.Execution == nil ||
		record.Execution.TaskID != tracked.ID || record.TaskID != tracked.ID || !record.State.Terminal() ||
		record.Unsettled || record.SessionSettled == nil || !*record.SessionSettled ||
		tracked.Result == nil || tracked.Result.Attempt != record.ID {
		return
	}
	usage := recoveredUsage(record)
	known := usage.Reported || usage.Tokens.Input != 0 || usage.Tokens.Output != 0 ||
		usage.Tokens.CachedRead != 0 || usage.Tokens.CachedWrite != 0
	for _, row := range tracked.Attempts {
		if row.ExecutionID != record.ID || row.TurnID != record.TurnID || row.ExecutionEpoch != record.Execution.Epoch {
			continue
		}
		if row.Open() || row.UsageKnown == nil || known && (!*row.UsageKnown || row.Tokens != usage.Tokens || row.Model != usage.Model) {
			return
		}
		s.executions.ResolveStopped(record.ID, *record.Execution)
		return
	}
}
