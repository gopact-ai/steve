package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Commit uploads missing blobs and obtains receiver-owned complete-package
// receipts before entering the ledger. A failure leaves content pinned for a
// safe retry, but cannot create a restorable authoritative manifest.
func (s *Store) Commit(ctx context.Context, snapshot Snapshot, targets []string) (Manifest, error) {
	if s.cfg.Records == nil {
		return Manifest{}, fmt.Errorf("%w: checkpoint record writer is not configured", ErrInvalid)
	}
	snapshot, id, refs, err := canonical(snapshot, s.cfg.Limits)
	if err != nil {
		return Manifest{}, err
	}
	if len(targets) > 256 {
		return Manifest{}, fmt.Errorf("%w: too many replica targets", ErrInvalid)
	}
	local, err := s.placement(ctx, snapshot.Scope, s.cfg.NodeID)
	if err != nil {
		return Manifest{}, err
	}
	places := map[string]Placement{s.cfg.NodeID: local}
	domains := map[string]bool{local.FailureDomain: true}
	for _, node := range targets {
		if _, seen := places[node]; seen {
			continue
		}
		placement, err := s.placement(ctx, snapshot.Scope, node)
		if err != nil {
			return Manifest{}, err
		}
		places[node], domains[placement.FailureDomain] = placement, true
	}
	if len(domains) < snapshot.RequiredCopies {
		return Manifest{}, fmt.Errorf("%w: need %d independent copies, have %d eligible targets", ErrIncomplete, snapshot.RequiredCopies, len(domains))
	}
	if len(places) > 256 {
		return Manifest{}, fmt.Errorf("%w: too many replica targets", ErrInvalid)
	}
	if len(places) > 1 && s.cfg.Replicas == nil {
		return Manifest{}, fmt.Errorf("%w: replica transport is not configured", ErrInvalid)
	}
	receipt, err := s.Prepare(ctx, snapshot)
	if err != nil {
		return Manifest{}, err
	}
	receipts := []Receipt{receipt}
	var failures []error
	for node, placement := range places {
		if node == s.cfg.NodeID {
			continue
		}
		receipt, err := s.replicate(ctx, node, snapshot, refs)
		if err != nil {
			failures = append(failures, fmt.Errorf("replica %s: %w", node, err))
			continue
		}
		if receipt.NodeID != node || receipt.SnapshotID != id || receipt.FailureDomain != placement.FailureDomain || receipt.StoredAt.IsZero() {
			return Manifest{}, fmt.Errorf("%w: replica returned a receipt for another package or node", ErrIntegrity)
		}
		receipts = append(receipts, receipt)
	}
	m, err := completeManifest(snapshot, receipts, s.cfg.Limits)
	if err != nil {
		return Manifest{}, errors.Join(err, errors.Join(failures...))
	}
	for _, receipt := range m.Receipts {
		current, err := s.placement(ctx, snapshot.Scope, receipt.NodeID)
		if err != nil {
			return Manifest{}, err
		}
		if current.FailureDomain != receipt.FailureDomain {
			return Manifest{}, fmt.Errorf("%w: replica placement changed", ErrPlacement)
		}
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := s.cfg.Records.RecordCheckpoint(ctx, cloneManifest(m)); err != nil {
		return Manifest{}, fmt.Errorf("checkpoint: commit manifest: %w", err)
	}
	s.mu.Lock()
	err = s.writeJSONLocked("manifests/"+m.ID+".json", m)
	s.mu.Unlock()
	if err != nil {
		// The ledger may already contain it. Repeating Commit is safe and
		// repairs this local index; the prepared package remains pinned.
		return Manifest{}, fmt.Errorf("checkpoint: ledger committed but local index needs retry: %w", err)
	}
	return cloneManifest(m), nil
}

func (s *Store) replicate(ctx context.Context, node string, snapshot Snapshot, refs []BlobRef) (Receipt, error) {
	for _, ref := range refs {
		has, err := s.cfg.Replicas.HasBlob(ctx, node, snapshot.Scope, ref)
		if err != nil && !errors.Is(err, ErrIntegrity) {
			return Receipt{}, err
		}
		if has {
			continue
		}
		file, err := s.openBlob(ctx, snapshot.Scope, ref)
		if err != nil {
			return Receipt{}, err
		}
		err = s.cfg.Replicas.PutBlob(ctx, node, snapshot.Scope, ref, file)
		closeErr := file.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return Receipt{}, err
		}
	}
	return s.cfg.Replicas.PrepareCheckpoint(ctx, node, snapshot)
}

// Verify consumes a manifest read from the authoritative ledger. Receipts are
// evidence of the commit-time durability contract; this check additionally
// verifies every local byte and the current node's placement permission.
func (s *Store) Verify(ctx context.Context, manifest Manifest) (VerifiedCheckpoint, error) {
	m, err := validateManifest(manifest, s.cfg.Limits)
	if err != nil {
		return VerifiedCheckpoint{}, err
	}
	receipt, err := s.Prepare(ctx, m.Snapshot)
	if err != nil {
		return VerifiedCheckpoint{}, err
	}
	return VerifiedCheckpoint{manifest: cloneManifest(m), nodeID: s.cfg.NodeID, receipt: receipt}, nil
}

// Fetch repairs a node's complete package from the ledger's recorded replicas.
// A receiver never adopts a partial transfer as a checkpoint.
func (s *Store) Fetch(ctx context.Context, manifest Manifest) (VerifiedCheckpoint, error) {
	m, err := validateManifest(manifest, s.cfg.Limits)
	if err != nil {
		return VerifiedCheckpoint{}, err
	}
	if _, err := s.placement(ctx, m.Snapshot.Scope, s.cfg.NodeID); err != nil {
		return VerifiedCheckpoint{}, err
	}
	_, _, refs, _ := canonical(m.Snapshot, s.cfg.Limits)
	s.mu.Lock()
	if err := s.checkOpenLocked(); err != nil {
		s.mu.Unlock()
		return VerifiedCheckpoint{}, err
	}
	for _, ref := range refs {
		s.pins[blobName(m.Snapshot.Scope, ref)]++
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, ref := range refs {
			name := blobName(m.Snapshot.Scope, ref)
			s.pins[name]--
			if s.pins[name] == 0 {
				delete(s.pins, name)
			}
		}
	}()
	for _, ref := range refs {
		has, err := s.HasBlob(ctx, m.Snapshot.Scope, ref)
		if err != nil && !errors.Is(err, ErrIntegrity) {
			return VerifiedCheckpoint{}, err
		}
		if has {
			continue
		}
		if s.cfg.Replicas == nil {
			return VerifiedCheckpoint{}, fmt.Errorf("%w: no replica transport", ErrIncomplete)
		}
		var failures []error
		fetched := false
		for _, receipt := range m.Receipts {
			if receipt.NodeID == s.cfg.NodeID {
				continue
			}
			placement, err := s.placement(ctx, m.Snapshot.Scope, receipt.NodeID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if placement.FailureDomain != receipt.FailureDomain {
				failures = append(failures, ErrPlacement)
				continue
			}
			if err := s.fetchBlob(ctx, receipt.NodeID, m.Snapshot.Scope, ref); err != nil {
				failures = append(failures, err)
				continue
			}
			fetched = true
			break
		}
		if !fetched {
			return VerifiedCheckpoint{}, fmt.Errorf("%w: no usable replica: %w", ErrIncomplete, errors.Join(failures...))
		}
	}
	return s.Verify(ctx, m)
}

func (s *Store) fetchBlob(ctx context.Context, node string, scope Scope, ref BlobRef) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	// A pipe's CloseWithError always returns nil, and the first close wins:
	// closing both ends on cancellation, on the sender's end and on the
	// receiver's end only tells the other side why the stream stopped.
	stop := context.AfterFunc(ctx, func() {
		_ = reader.CloseWithError(ctx.Err())
		_ = writer.CloseWithError(ctx.Err())
	})
	defer stop()
	done := make(chan error, 1)
	go func() {
		err := s.cfg.Replicas.GetBlob(ctx, node, scope, ref, writer)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	err := s.PutBlob(ctx, scope, ref, reader)
	_ = reader.CloseWithError(err)
	cancel()
	return errors.Join(err, <-done)
}
