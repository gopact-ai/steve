package nodewire

import "github.com/gopact-ai/steve/internal/artifact/ops"

// ArtifactRequest and ArtifactResult are the artifact domain contract carried
// by StreamArtifact. The wire depends on the domain, not the other way around.
type ArtifactOp = ops.Kind
type ArtifactRequest = ops.Request
type ArtifactResult = ops.Result
type OperationFailure = ops.Failure
type SnapshotLimits = ops.Limits

const (
	ArtifactInit          = ops.Init
	ArtifactSnapshot      = ops.Snapshot
	ArtifactCheckout      = ops.Checkout
	ArtifactHas           = ops.Has
	ArtifactBundle        = ops.Bundle
	ArtifactUnbundle      = ops.Unbundle
	ArtifactMerge         = ops.Merge
	ArtifactApply         = ops.Apply
	ArtifactChanged       = ops.Changed
	ArtifactRemove        = ops.Remove
	ArtifactListWorktrees = ops.ListWorktrees
	ArtifactPathState     = ops.PathState
	ArtifactWritePath     = ops.WritePath
)

type ArtifactReply struct {
	Result ArtifactResult    `json:"result"`
	Error  *OperationFailure `json:"error,omitempty"`
}
