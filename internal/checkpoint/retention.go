package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"time"
)

var ErrRetired = errors.New("checkpoint: retained object is retired")

// RetainedBlob binds an exact receipt key (not a content ID) to scoped bytes.
// Owner is a bounded, opaque descriptor used by the owner to verify release.
type RetainedBlob struct {
	ID string `json:"id"`
	// The caller must bind Sequence into ID's identity. Changing this number
	// alone must never turn a released receipt into a new upload.
	Sequence uint64  `json:"sequence"`
	Scope    Scope   `json:"scope"`
	Blob     BlobRef `json:"blob"`
	Owner    string  `json:"owner"`
}

// HasRetained verifies an exact active admission, including its immutable
// descriptor. It does not treat a missing marker or an old floor as a release
// decision; owners use this only to recognize previously admitted promises.
func (s *Store) HasRetained(r RetainedBlob) (bool, error) {
	if err := s.validateRetention(r); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, err := s.checkRetentionLocked(r)
	return !at.IsZero(), err
}

type RetentionGCResult struct {
	GCResult
	// Released lists exact identities whose replay fence is durable and whose
	// marker no longer occupies quota. In-flight streams defer confirmation.
	Released []string `json:"released,omitempty"`
}

type retentionRecord struct {
	Retention RetainedBlob `json:"retention"`
	StoredAt  time.Time    `json:"stored_at"`
}

func (s *Store) validateRetention(r RetainedBlob) error {
	if !validDigest(r.ID) || r.Sequence == 0 || int64(len(r.Owner)) > s.cfg.Limits.MaxManifestBytes/2 {
		return ErrInvalid
	}
	if err := validateScope(r.Scope); err != nil {
		return err
	}
	return validateBlob(r.Blob, s.cfg.Limits)
}

func (s *Store) checkRetentionLocked(r RetainedBlob) (time.Time, error) {
	var storedAt time.Time
	floor, exists, err := s.retentionFloorLocked()
	if err != nil {
		return storedAt, err
	}
	for _, dir := range []string{"retired", "retained"} {
		var existing retentionRecord
		ok, err := s.readJSONLocked(dir+"/"+r.ID+".json", &existing)
		if err != nil {
			return storedAt, err
		}
		if ok {
			if !exists || existing.Retention != r || existing.StoredAt.IsZero() {
				return storedAt, ErrIntegrity
			}
			if dir == "retired" {
				return storedAt, ErrRetired
			}
			storedAt = existing.StoredAt
		}
	}
	if storedAt.IsZero() && r.Sequence <= floor {
		return storedAt, ErrRetired
	}
	return storedAt, nil
}

// PutRetainedBlob protects the upload across streaming, ordinary GC and
// restart before acknowledging it. An explicit concurrent retirement wins:
// the upload may finish storing bytes, but cannot return a usable receipt.
func (s *Store) PutRetainedBlob(ctx context.Context, r RetainedBlob, source io.Reader) (time.Time, error) {
	if err := s.validateRetention(r); err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	if err := s.advanceRetentionFloorLocked(0); err != nil {
		s.mu.Unlock()
		return time.Time{}, err
	}
	at, err := s.checkRetentionLocked(r)
	if err != nil {
		s.mu.Unlock()
		return time.Time{}, err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	// Account and persist protection before consuming the stream. A later
	// quota failure leaves an identifiable, explicitly releasable upload,
	// rather than an untracked complete blob.
	if err := s.writeJSONLocked("retained/"+r.ID+".json", retentionRecord{Retention: r, StoredAt: at}); err != nil {
		s.mu.Unlock()
		return time.Time{}, err
	}
	name := blobName(r.Scope, r.Blob)
	s.pins[name]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.pins[name]--
		if s.pins[name] == 0 {
			delete(s.pins, name)
		}
	}()
	if err := s.PutBlob(ctx, r.Scope, r.Blob, source); err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	at, err = s.checkRetentionLocked(r)
	if err != nil {
		return time.Time{}, err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	err = s.writeJSONLocked("retained/"+r.ID+".json", retentionRecord{Retention: r, StoredAt: at})
	return at, err
}

// retentionsLocked validates the complete protection index before any
// collection. A malformed unrelated marker must not become a missing root.
func (s *Store) retentionsLocked(ctx context.Context) (map[string]RetainedBlob, map[string]RetainedBlob, error) {
	active, retired := map[string]RetainedBlob{}, map[string]RetainedBlob{}
	for _, dir := range []string{"retained", "retired"} {
		entries, err := fs.ReadDir(s.root.FS(), dir)
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			var record retentionRecord
			ok, err := s.readJSONLocked(dir+"/"+entry.Name(), &record)
			if err != nil {
				return nil, nil, err
			}
			r := record.Retention
			if !ok || record.StoredAt.IsZero() || !entry.Type().IsRegular() || s.validateRetention(r) != nil || entry.Name() != r.ID+".json" {
				return nil, nil, fmt.Errorf("%w: retention %s/%s", ErrIntegrity, dir, entry.Name())
			}
			if dir == "retained" {
				active[r.ID] = r
			} else {
				if old, ok := active[r.ID]; ok && old != r {
					return nil, nil, ErrIntegrity
				}
				retired[r.ID] = r
				delete(active, r.ID)
			}
		}
	}
	if len(active)+len(retired) != 0 {
		if _, exists, err := s.retentionFloorLocked(); err != nil || !exists {
			return nil, nil, errors.Join(ErrIntegrity, err)
		}
	}
	return active, retired, nil
}

// CollectRetained applies an owner's already committed retirement decisions.
// It never infers retirement from age, missing ledger rows or missing local
// markers. Callers must fence publication of these IDs before calling.
//
// All identities are durably retired before any bytes are removed. Retrying
// the same decisions after cancellation/crash is safe; other retained owners,
// prepared checkpoints and in-flight operations keep shared blobs alive.
// Required receipts must still have exact active markers under the same lock;
// damage to that index cannot turn a live ledger promise into missing protection.
func (s *Store) CollectRetained(ctx context.Context, candidates, required []RetainedBlob) (RetentionGCResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return RetentionGCResult{}, err
	}
	active, retired, err := s.retentionsLocked(ctx)
	if err != nil {
		return RetentionGCResult{}, err
	}
	needed := map[string]bool{}
	for _, r := range required {
		if s.validateRetention(r) != nil || active[r.ID] != r {
			return RetentionGCResult{}, fmt.Errorf("%w: missing active receipt %s", ErrIntegrity, r.ID)
		}
		needed[r.ID] = true
	}
	if _, err := s.gcKeepLocked(ctx); err != nil {
		return RetentionGCResult{}, err
	}
	unique := map[string]RetainedBlob{}
	var floor uint64
	for _, r := range candidates {
		if s.validateRetention(r) != nil || needed[r.ID] {
			return RetentionGCResult{}, ErrIntegrity
		}
		for _, entries := range []map[string]RetainedBlob{active, retired, unique} {
			if old, ok := entries[r.ID]; ok && old != r {
				return RetentionGCResult{}, ErrIntegrity
			}
		}
		unique[r.ID] = r
		floor = max(floor, r.Sequence)
	}
	if len(unique) == 0 {
		return RetentionGCResult{}, nil
	}
	// Persist the compact replay fence BEFORE removing any exact marker,
	// including releases for targets where the upload never arrived.
	if err := s.advanceRetentionFloorLocked(floor); err != nil {
		return RetentionGCResult{}, err
	}
	if err := s.retireMarkersLocked(ctx, unique, active, retired); err != nil {
		return RetentionGCResult{}, err
	}
	allowed := map[string]bool{}
	for _, r := range unique {
		allowed[blobName(r.Scope, r.Blob)] = true
	}
	result, err := s.gcLocked(ctx, allowed)
	if err != nil {
		return RetentionGCResult{GCResult: result}, err
	}
	released, err := s.compactRetiredLocked(ctx, unique)
	return RetentionGCResult{GCResult: result, Released: released}, err
}

func (s *Store) retireMarkersLocked(ctx context.Context, unique, active, retired map[string]RetainedBlob) error {
	for _, r := range unique {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, present := active[r.ID]; present {
			if _, done := retired[r.ID]; done {
				continue
			}
			from, to := "retained/"+r.ID+".json", "retired/"+r.ID+".json"
			// Rename reserves no extra quota. A full store must still be
			// able to apply a valid release. Both directories are synced
			// before any blob can be removed.
			if err := s.root.Rename(from, to); err != nil {
				return err
			}
			s.sizes[to] = s.sizes[from]
			delete(s.sizes, from)
		}
	}
	if err := s.syncDir("retired"); err != nil {
		return err
	}
	// The tombstone is durable. Its former active marker can now be removed
	// without losing the fence, even if this process stops between the two.
	for id := range retired {
		name := "retained/" + id + ".json"
		if err := s.root.Remove(name); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		s.used -= s.sizes[name]
		delete(s.sizes, name)
		s.objects--
	}
	return s.syncDir("retained")
}

func (s *Store) compactRetiredLocked(ctx context.Context, candidates map[string]RetainedBlob) ([]string, error) {
	if err := s.syncCollectionBlobsLocked(candidates); err != nil {
		return nil, err
	}
	var released []string
	for id, r := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := blobName(r.Scope, r.Blob)
		// Keep the retry candidate until an admitted stream finishes. It may
		// still install bytes, but its post-stream admission check will fail.
		if s.pins[name] != 0 || s.uploads[name] != nil {
			continue
		}
		name = "retired/" + id + ".json"
		err := s.root.Remove(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			s.used -= s.sizes[name]
			delete(s.sizes, name)
			s.objects--
		}
		released = append(released, id)
	}
	if err := s.syncDir("retired"); err != nil {
		return nil, err
	}
	return released, nil
}

func (s *Store) syncCollectionBlobsLocked(candidates map[string]RetainedBlob) error {
	// A previous unlink may have succeeded while its directory fsync failed.
	// Retrying absent bytes must make the absence durable before forgetting
	// their final cleanup candidate. Many receipts can share one directory.
	dirs := map[string]bool{}
	for _, r := range candidates {
		dirs[path.Dir(blobName(r.Scope, r.Blob))] = true
	}
	for dir := range dirs {
		if err := s.syncDir(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.syncDir("blobs")
}

// Retained returns a validated inventory, including interrupted releases.
// It is not deletion authorization; the owner must obtain a durable decision
// for each exact ID before supplying it to CollectRetained.
func (s *Store) Retained(ctx context.Context) ([]RetainedBlob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	active, retired, err := s.retentionsLocked(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RetainedBlob, 0, len(active)+len(retired))
	for _, r := range active {
		out = append(out, r)
	}
	for _, r := range retired {
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) retainedKeepLocked(ctx context.Context, keep map[string]bool) error {
	active, _, err := s.retentionsLocked(ctx)
	if err != nil {
		return err
	}
	for _, r := range active {
		keep[blobName(r.Scope, r.Blob)] = true
	}
	return nil
}
