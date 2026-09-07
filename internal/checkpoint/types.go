// Package checkpoint stores portable execution checkpoints separately from
// native agent sessions. Content is explicit, bounded and content-addressed;
// only complete packages with durable independent copies enter the ledger.
package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

var (
	ErrInvalid    = errors.New("checkpoint: invalid input")
	ErrQuota      = errors.New("checkpoint: quota exceeded")
	ErrIncomplete = errors.New("checkpoint: content is incomplete")
	ErrIntegrity  = errors.New("checkpoint: content integrity check failed")
	ErrPlacement  = errors.New("checkpoint: placement refused")
)

// Scope is the data classification of the entire package, including context.
// Node IDs must be stable identities, never the moving coordinator alias.
type Scope struct {
	ProjectID  string `json:"project_id"`
	Level      string `json:"level"`
	HomeNodeID string `json:"home_node_id"`
}

type Placement struct {
	// FailureDomain identifies a physical failure boundary. Two node IDs on
	// the same machine cannot satisfy two independent copies.
	FailureDomain string
}

// PlacementPolicy must check project classification (including sealed home),
// current node membership and its permission to store this project's data.
// It is required on both sending and receiving stores; there is no allow-all
// default. Errors should describe the refusal without including credentials.
type PlacementPolicy interface {
	CheckpointPlacement(context.Context, Scope, string) (Placement, error)
}

type BlobRef struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func Reference(content []byte) BlobRef {
	sum := sha256.Sum256(content)
	return BlobRef{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))}
}

// Source preserves the logical session and task while naming the old writer.
// ExecutionEpoch is the writer fence, separate from task.ExecutionToken.Epoch:
// TaskEpoch is the user's authorization generation and is not bumped merely
// because the coordinator changes. Native session IDs and AgentToken are not
// portable context and must not be serialized into a checkpoint.
type Source struct {
	TaskID         string `json:"task_id"`
	SessionID      string `json:"session_id"`
	AttemptID      string `json:"attempt_id"`
	TurnID         string `json:"turn_id"`
	NodeID         string `json:"node_id"`
	ExecutionEpoch uint64 `json:"execution_epoch"`
	TaskEpoch      uint64 `json:"task_epoch"`
}

type Cursors struct {
	InputAccepted   uint64 `json:"input_accepted"`
	OutputPublished uint64 `json:"output_published"`
}

// File names a selected regular file within its material/workspace root.
// Absolute paths, symlinks and automatic directory traversal are unsupported.
// The producer must explicitly select and sanitize portable content; credentials
// and machine-local login state belong to the destination's capability checks.
type File struct {
	Path       string  `json:"path"`
	Blob       BlobRef `json:"blob"`
	Executable bool    `json:"executable,omitempty"`
}

type Workspace struct {
	ID           string `json:"id"`
	BaseArtifact string `json:"base_artifact,omitempty"`
	Files        []File `json:"files"`
}

type ExternalAction struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// ReconcileRef identifies an operation record, never a credential or a
	// bearer URL. Unknown actions must be reconciled before a new attempt.
	ReconcileRef string `json:"reconcile_ref,omitempty"`
}

// Snapshot is a complete index over shared blobs. Unchanged files keep their
// digest across snapshots, so transfer is incremental without parent chains.
// Context contains a sanitized portable goal/findings/history document rather
// than an opaque native session database. CreatedAt is caller-owned for retry
// stability. RequiredCopies includes the local copy and must be explicit.
type Snapshot struct {
	Version        int              `json:"version"`
	Scope          Scope            `json:"scope"`
	Source         Source           `json:"source"`
	Cursors        Cursors          `json:"cursors"`
	Context        BlobRef          `json:"context"`
	Workspace      Workspace        `json:"workspace"`
	Materials      []File           `json:"materials,omitempty"`
	UnknownActions []ExternalAction `json:"unknown_actions,omitempty"`
	RequiredCopies int              `json:"required_copies"`
	CreatedAt      time.Time        `json:"created_at"`
}

type Receipt struct {
	SnapshotID    string    `json:"snapshot_id"`
	NodeID        string    `json:"node_id"`
	FailureDomain string    `json:"failure_domain"`
	StoredAt      time.Time `json:"stored_at"`
}

type Manifest struct {
	ID       string    `json:"id"`
	Snapshot Snapshot  `json:"snapshot"`
	Receipts []Receipt `json:"receipts"`
}

// RecordWriter commits an immutable checkpoint manifest to the authoritative
// replicated ledger. It must atomically check the current execution authority
// and make repeated identical IDs idempotent; a lost response can be retried.
// It must not expose a manifest before this transaction has committed.
type RecordWriter interface {
	RecordCheckpoint(context.Context, Manifest) error
}

// ReplicaTransport uses authenticated node identities. PrepareCheckpoint must
// return the receiving Store.Prepare receipt only after durable storage of all
// referenced content. A transport completion or a sender's assertion is not a
// receipt. Repeated PutBlob calls are idempotent and verify digest and size.
type ReplicaTransport interface {
	HasBlob(context.Context, string, Scope, BlobRef) (bool, error)
	PutBlob(context.Context, string, Scope, BlobRef, io.Reader) error
	GetBlob(context.Context, string, Scope, BlobRef, io.Writer) error
	PrepareCheckpoint(context.Context, string, Snapshot) (Receipt, error)
}

type Limits struct {
	MaxBlobBytes     int64
	MaxBytes         int64
	MaxSnapshotBytes int64
	MaxManifestBytes int64
	MaxFiles         int
	MaxObjects       int
}

func (l Limits) defaults() Limits {
	if l.MaxBlobBytes == 0 {
		l.MaxBlobBytes = 64 << 20
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = 2 << 30
	}
	if l.MaxSnapshotBytes == 0 {
		l.MaxSnapshotBytes = 512 << 20
	}
	if l.MaxManifestBytes == 0 {
		l.MaxManifestBytes = 4 << 20
	}
	if l.MaxFiles == 0 {
		l.MaxFiles = 8192
	}
	if l.MaxObjects == 0 {
		l.MaxObjects = 65536
	}
	return l
}

type Config struct {
	Dir      string
	NodeID   string
	Limits   Limits
	Policy   PlacementPolicy
	Records  RecordWriter
	Replicas ReplicaTransport
}

// VerifiedCheckpoint can only be produced after local content validation.
// Treat it as a point-in-time result and recheck before materializing execution.
type VerifiedCheckpoint struct {
	manifest Manifest
	nodeID   string
	receipt  Receipt
}

func (v VerifiedCheckpoint) Manifest() Manifest { return cloneManifest(v.manifest) }
func (v VerifiedCheckpoint) NodeID() string     { return v.nodeID }

// LocalReceipt lets the replication catalog record a repaired copy without
// modifying the original immutable checkpoint manifest.
func (v VerifiedCheckpoint) LocalReceipt() Receipt { return v.receipt }

type Restored struct {
	Directory string
	Context   string
	Workspace string
	Materials string
}

type GCResult struct {
	Blobs int
	Bytes int64
}
