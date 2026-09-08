package node

import (
	"time"

	"github.com/gopact-ai/steve/internal/view"
)

// Progress checkpoints are bounded by time, not token count. Commands,
// permissions and terminal results still commit synchronously and include
// the most recent progress even when this timer has not fired yet.
const sessionProgressEvery = 100 * time.Millisecond

func (one *ownedSession) updateProgress(commandID string, progress view.Progress) error {
	one.mu.Lock()
	defer one.mu.Unlock()
	if one.failure != nil {
		return one.failure
	}
	if one.record.CurrentCommand != commandID || !one.runningLocked() {
		return nil
	}
	previous := one.record.State.Progress
	one.pendingProgress = &progress
	first := (previous.Answer == "" && progress.Answer != "") || (previous.Reasoning == "" && progress.Reasoning != "")
	if first || progressToolsChanged(previous.Tools, progress.Tools) {
		return one.commitLocked(one.copyLocked())
	}
	if one.progressTimer == nil {
		var timer *time.Timer
		timer = time.AfterFunc(sessionProgressEvery, func() {
			one.mu.Lock()
			if one.progressTimer != timer {
				one.mu.Unlock()
				return
			}
			one.progressTimer = nil
			err := one.commitLocked(one.copyLocked())
			host, generation := one.host, one.record.Generation
			one.mu.Unlock()
			if err != nil && host != nil {
				host.Abort(generation)
			}
		})
		one.progressTimer = timer
	}
	return nil
}

func progressToolsChanged(before, after []view.Tool) bool {
	if len(before) != len(after) {
		return true
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Status != after[i].Status || progressToolsChanged(before[i].Children, after[i].Children) {
			return true
		}
	}
	return false
}
