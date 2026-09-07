package workflowstore

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gopact-ai/gopact/workflow"
)

const maxSequence = int64(^uint64(0) >> 1)
const maxPayload = 4 << 20

func validID(id string) bool {
	return id != "" && len(id) <= 191 && utf8.ValidString(id) && !strings.ContainsRune(id, 0) && !strings.HasSuffix(id, " ")
}
func validLineage(runID string, sequence int64, revision string) bool {
	return sequence >= 0 && (runID == "") == (sequence == 0) && (runID != "" || revision == "")
}
func isTerminal(status workflow.CheckpointStatus) bool {
	return status == workflow.CheckpointCompleted || status == workflow.CheckpointFailed || status == workflow.CheckpointCanceled || status == workflow.CheckpointTerminated
}

func validateCheckpoint(record workflow.CheckpointRecord) error {
	if record.ID == "" || !validID(record.SessionID) || !validID(record.RunID) || record.WorkflowName == "" || record.TopologyVersion == "" || record.SchemaVersion <= 0 {
		return fmt.Errorf("%w: checkpoint identity is incomplete", workflow.ErrInvalidCheckpoint)
	}
	if record.ConfirmedSequence < 0 || record.PendingSequence < 0 || record.LeaseDuration < 0 || len(record.Payload) == 0 || len(record.Payload) > maxPayload || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: invalid checkpoint payload, sequence, lease or time", workflow.ErrInvalidCheckpoint)
	}
	if !validLineage(record.SourceRunID, record.SourceEventSeq, record.SourceRevisionID) {
		return fmt.Errorf("%w: incomplete checkpoint lineage", workflow.ErrInvalidCheckpoint)
	}
	if record.Status != workflow.CheckpointRunning && record.Status != workflow.CheckpointInterrupted && !isTerminal(record.Status) {
		return fmt.Errorf("%w: unknown checkpoint status", workflow.ErrInvalidCheckpoint)
	}
	if record.ReplayStatus != workflow.ReplayUnknown && record.ReplayStatus != workflow.ReplaySafe && record.ReplayStatus != workflow.ReplayUnsafe {
		return fmt.Errorf("%w: invalid replay status", workflow.ErrInvalidCheckpoint)
	}
	return nil
}

func sameIdentity(a, b workflow.CheckpointRecord) bool {
	return a.ID == b.ID && a.SessionID == b.SessionID && a.RunID == b.RunID && a.SourceRunID == b.SourceRunID && a.SourceEventSeq == b.SourceEventSeq && a.SourceRevisionID == b.SourceRevisionID && a.WorkflowName == b.WorkflowName && a.TopologyVersion == b.TopologyVersion && a.SchemaVersion == b.SchemaVersion
}
