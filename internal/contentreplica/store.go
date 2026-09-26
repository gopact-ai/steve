package contentreplica

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/ledger"
)

// Store uses the checkpoint engine's streaming, confinement, quota and fsync
// primitives in a separate content directory. Every exact upload receipt is
// retained until an explicit ledger release, including lost publish responses.
type Store struct {
	blobs  *checkpoint.Store
	node   string
	policy PlacementPolicy
	limit  int64
	book   *ledger.Ledger
}

func Open(cfg StoreConfig) (*Store, error) {
	if cfg.Limits.MaxBlobBytes == 0 {
		cfg.Limits.MaxBlobBytes = DefaultMaxObjectBytes
	}
	if cfg.Policy == nil {
		return nil, fmt.Errorf("%w: placement policy required", ErrInvalid)
	}
	blobs, err := checkpoint.OpenRetained(checkpoint.Config{Dir: cfg.Dir, NodeID: cfg.NodeID, Policy: cfg.Policy, Limits: cfg.Limits})
	if err != nil {
		return nil, err
	}
	return &Store{blobs: blobs, node: cfg.NodeID, policy: cfg.Policy, limit: cfg.Limits.MaxBlobBytes, book: cfg.Ledger}, nil
}

func (s *Store) Close() error { return s.blobs.Close() }

func (s *Store) Put(ctx context.Context, upload Upload, content io.Reader) (Receipt, error) {
	object := upload.Object
	if uploadSequence(upload.ID) == 0 {
		return Receipt{}, ErrInvalid
	}
	if err := validateObject(object, s.limit); err != nil {
		return Receipt{}, err
	}
	where, err := placement(ctx, s.policy, object.Scope, s.node)
	if err != nil {
		return Receipt{}, err
	}
	retained := retainedUpload(upload, s.node, where.FailureDomain)
	if err := s.checkUpload(ctx, upload, retained); err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{UploadID: upload.ID, ObjectID: object.ID(), NodeID: s.node, FailureDomain: where.FailureDomain}
	at, err := s.blobs.PutRetainedBlob(ctx, retained, content)
	if err != nil {
		return Receipt{}, err
	}
	if err := s.checkUpload(ctx, upload, retained); err != nil {
		return Receipt{}, err
	}
	current, err := placement(ctx, s.policy, object.Scope, s.node)
	if errors.Is(err, ErrUnavailable) {
		return Receipt{}, err
	}
	if err != nil || current.FailureDomain != where.FailureDomain {
		return Receipt{}, fmt.Errorf("%w: placement changed: %v", ErrPlacement, err)
	}
	receipt.StoredAt = at
	return receipt, nil
}

func (s *Store) checkUpload(ctx context.Context, upload Upload, retained checkpoint.RetainedBlob) error {
	if s.book == nil {
		if upload.Object.Base != "" {
			return ErrIncomplete
		}
		return nil
	}
	return s.book.Read(ctx, func(tx *ledger.ReadTx) error {
		var raw string
		err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, uploadKind, upload.ID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			if upload.Object.Base != "" {
				return ErrIncomplete
			}
			high, err := readUploadClock(func(q string, args ...any) retentionRow { return tx.QueryRow(q, args...) })
			if err != nil {
				return err
			}
			if uploadSequence(upload.ID) <= high {
				// An empty/new receiver directory must not re-admit pruned
				// IDs merely because its local floor starts at zero. Existing
				// exact unknown promises are exceptions, never evictions.
				exists, err := s.blobs.HasRetained(retained)
				if err != nil {
					return err
				}
				if !exists {
					return ErrReleased
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		u, err := decodeUpload(upload.ID, []byte(raw))
		if err != nil {
			return err
		}
		if u.Upload != upload || !slices.Contains(u.Targets, s.node) {
			return ErrIntegrity
		}
		if u.State == "aborted" {
			return ErrReleased
		}
		if u.State != "pending" && !slices.ContainsFunc(u.Receipts, func(r Receipt) bool { return r.NodeID == s.node }) {
			return ErrReleased
		}
		key := (Receipt{UploadID: upload.ID, ObjectID: upload.Object.ID(), NodeID: s.node}).Key()
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM bindings WHERE kind = ? AND id = ?`, releaseKind, key).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrReleased
		}
		return nil
	})
}

type retainedOwner struct {
	Upload        Upload `json:"upload"`
	FailureDomain string `json:"failure_domain"`
}

func retainedUpload(upload Upload, node, domain string) checkpoint.RetainedBlob {
	r := Receipt{UploadID: upload.ID, ObjectID: upload.Object.ID(), NodeID: node}
	owner, _ := json.Marshal(retainedOwner{Upload: upload, FailureDomain: domain})
	return checkpoint.RetainedBlob{ID: r.Key(), Sequence: uploadSequence(upload.ID), Scope: upload.Object.Scope, Blob: upload.Object.Blob, Owner: string(owner)}
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
