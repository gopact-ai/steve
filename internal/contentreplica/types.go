// Package contentreplica stores durable copies of material and Git-bundle
// content before their references enter the replicated application ledger.
package contentreplica

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
)

const (
	Material                    = "material"
	PluginPackage               = "plugin-package"
	GitBundle                   = "git-bundle"
	Replicated                  = "replicated"
	SingleNode                  = "single_node"
	SealedHome                  = "sealed_home"
	DefaultMaxObjectBytes int64 = 512 << 20
)

var (
	ErrInvalid    = errors.New("invalid content replica")
	ErrIncomplete = errors.New("content has insufficient durable replicas")
	ErrIntegrity  = errors.New("content replica integrity check failed")
	ErrPlacement  = errors.New("content replica placement refused")
	ErrTooLarge   = errors.New("content exceeds replication limit")
)

type Scope = checkpoint.Scope
type BlobRef = checkpoint.BlobRef
type Placement = checkpoint.Placement
type PlacementPolicy = checkpoint.PlacementPolicy

type Object struct {
	Scope Scope   `json:"scope"`
	Kind  string  `json:"kind"`
	Key   string  `json:"key"`
	Blob  BlobRef `json:"blob"`
}

func (o Object) ID() string {
	raw, _ := json.Marshal(o)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type Receipt struct {
	ObjectID      string    `json:"object_id"`
	NodeID        string    `json:"node_id"`
	FailureDomain string    `json:"failure_domain"`
	StoredAt      time.Time `json:"stored_at"`
}

type Manifest struct {
	ID             string    `json:"id"`
	Object         Object    `json:"object"`
	RequiredCopies int       `json:"required_copies"`
	Receipts       []Receipt `json:"receipts"`
	Protection     string    `json:"protection"`
}

// Recoverable reports the receipt-time machine-loss protection. Single-node
// and sealed-home manifests intentionally make no cross-machine promise.
func (m Manifest) Recoverable() bool {
	return m.Protection == Replicated && m.RequiredCopies >= 2 && m.Complete()
}
func (m Manifest) Complete() bool { return validateManifest(m, math.MaxInt64-1) == nil }

// BinaryStore returns a receiver-owned receipt after digest verification and
// durable storage. A successful write to a transport socket is not a receipt.
// Acknowledged objects must remain retained until explicit ledger-aware removal.
type BinaryStore interface {
	Put(context.Context, Object, io.Reader) (Receipt, error)
	Get(context.Context, Object, io.Writer) error
}

// Transport authenticates the addressed receiver and binds Receipt.NodeID to
// its authenticated identity. In-process and mTLS adapters share this contract.
type Transport interface {
	Put(context.Context, string, Object, io.Reader) (Receipt, error)
	Get(context.Context, string, Object, io.Writer) error
}

// Replicator is consumed by material and artifact stores. Prepare never
// publishes a ledger reference; the owner commits it with its own metadata.
type Replicator interface {
	CheckLocal(context.Context, string) (Scope, error)
	Prepare(context.Context, string, string, string, BlobRef, io.ReadSeeker) (Manifest, error)
	// Read returns a manifest including a newly repaired local receipt. The
	// consumer persists that receipt before treating its cache as restored.
	Read(context.Context, Manifest, io.Writer) (Manifest, error)
}

type Config struct {
	NodeID string
	Local  BinaryStore
	Remote Transport
	Policy PlacementPolicy
	// Scope resolves the actual shared project classification and physical home.
	// Unknown/global/unbound material must be rejected or explicitly classified;
	// it must never default to public.
	Scope func(context.Context, string) (Scope, error)
	// Members returns committed configured node identities, including offline
	// members. An unavailable replica cannot silently turn two copies into one.
	Members        func(context.Context) ([]string, error)
	MaxObjectBytes int64
}

type StoreConfig struct {
	Dir    string
	NodeID string
	Policy PlacementPolicy
	Limits checkpoint.Limits
}
