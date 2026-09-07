package contentreplica

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
)

// Store uses the checkpoint engine's streaming, confinement, quota and fsync
// primitives in a separate content directory. It deliberately exposes no GC:
// acknowledged materials and bundles remain retained, including after a lost
// ledger response, until a future explicit ledger-aware retention operation.
type Store struct {
	blobs  *checkpoint.Store
	node   string
	policy PlacementPolicy
	limit  int64
}

func Open(cfg StoreConfig) (*Store, error) {
	if cfg.Limits.MaxBlobBytes == 0 {
		cfg.Limits.MaxBlobBytes = DefaultMaxObjectBytes
	}
	if cfg.Policy == nil {
		return nil, fmt.Errorf("%w: placement policy required", ErrInvalid)
	}
	blobs, err := checkpoint.Open(checkpoint.Config{Dir: cfg.Dir, NodeID: cfg.NodeID, Policy: cfg.Policy, Limits: cfg.Limits})
	if err != nil {
		return nil, err
	}
	return &Store{blobs: blobs, node: cfg.NodeID, policy: cfg.Policy, limit: cfg.Limits.MaxBlobBytes}, nil
}

func (s *Store) Close() error { return s.blobs.Close() }

func (s *Store) Put(ctx context.Context, object Object, content io.Reader) (Receipt, error) {
	if err := validateObject(object, s.limit); err != nil {
		return Receipt{}, err
	}
	if _, err := placement(ctx, s.policy, object.Scope, s.node); err != nil {
		return Receipt{}, err
	}
	if err := s.blobs.PutBlob(ctx, object.Scope, object.Blob, content); err != nil {
		return Receipt{}, err
	}
	where, err := placement(ctx, s.policy, object.Scope, s.node)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{ObjectID: object.ID(), NodeID: s.node, FailureDomain: where.FailureDomain, StoredAt: time.Now().UTC()}, nil
}

func (s *Store) Get(ctx context.Context, object Object, into io.Writer) error {
	if err := validateObject(object, s.limit); err != nil {
		return err
	}
	if _, err := placement(ctx, s.policy, object.Scope, s.node); err != nil {
		return err
	}
	return s.blobs.ReadBlob(ctx, object.Scope, object.Blob, into)
}
