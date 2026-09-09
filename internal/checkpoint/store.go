package checkpoint

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu              sync.Mutex
	root            *os.Root
	lock            *os.File
	cfg             Config
	used            int64
	objects         int
	reservedBytes   int64
	reservedObjects int
	sizes           map[string]int64
	uploads         map[string]chan struct{}
	closed          bool
	pins            map[string]int
}

func Open(cfg Config) (*Store, error) {
	cfg.Limits = cfg.Limits.defaults()
	if cfg.Dir == "" || !validID(cfg.NodeID) || cfg.Policy == nil {
		return nil, fmt.Errorf("%w: directory, stable node ID and placement policy are required", ErrInvalid)
	}
	l := cfg.Limits
	if l.MaxBlobBytes < 1 || l.MaxBlobBytes == int64(^uint64(0)>>1) || l.MaxBytes < 1 || l.MaxSnapshotBytes < 1 || l.MaxManifestBytes < 1 || l.MaxManifestBytes > 64<<20 || l.MaxFiles < 1 || l.MaxFiles > 1<<20 || l.MaxObjects < 1 {
		return nil, fmt.Errorf("%w: invalid storage limits", ErrInvalid)
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("checkpoint: create store: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: open store: %w", err)
	}
	s := &Store{root: root, cfg: cfg, pins: map[string]int{}, sizes: map[string]int64{}, uploads: map[string]chan struct{}{}}
	failed := true
	defer func() {
		if failed {
			// Open reports why it failed; releasing the lock file and
			// the root on the way out has nothing to add to that.
			if s.lock != nil {
				_ = s.lock.Close()
			}
			_ = root.Close()
		}
	}()
	if info, err := root.Lstat("store.lock"); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: invalid lock file", ErrInvalid)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.lock, err = root.OpenFile("store.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockStore(s.lock); err != nil {
		return nil, fmt.Errorf("checkpoint: store is already open: %w", err)
	}
	for _, name := range []string{"blobs", "prepared", "manifests", "tmp"} {
		if err := root.MkdirAll(name, 0o700); err != nil {
			return nil, err
		}
	}
	// Only incomplete temporary uploads are discarded after a crash. Prepared
	// packages stay pinned even if the ledger commit response was lost.
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: unexpected non-regular store entry", ErrInvalid)
		}
		if strings.HasPrefix(name, "tmp/") {
			return root.Remove(name)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > l.MaxBytes-s.used {
			return fmt.Errorf("%w: existing store exceeds limit", ErrQuota)
		}
		s.used += info.Size()
		if name != "store.lock" {
			s.sizes[name] = info.Size()
			s.objects++
			if s.objects > l.MaxObjects {
				return ErrQuota
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.syncDir("."); err != nil {
		return nil, err
	}
	failed = false
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.lock.Close(), s.root.Close())
}

func scopeID(scope Scope) string {
	// Scope is three plain strings; encoding it cannot fail.
	raw, _ := json.Marshal(scope)
	return Reference(raw).SHA256
}

func blobName(scope Scope, ref BlobRef) string { return path.Join("blobs", scopeID(scope), ref.SHA256) }

func (s *Store) placement(ctx context.Context, scope Scope, node string) (Placement, error) {
	if err := ctx.Err(); err != nil {
		return Placement{}, err
	}
	if err := validateScope(scope); err != nil {
		return Placement{}, err
	}
	if !validID(node) {
		return Placement{}, fmt.Errorf("%w: stable node identity required", ErrInvalid)
	}
	p, err := s.cfg.Policy.CheckpointPlacement(ctx, scope, node)
	if err != nil {
		return Placement{}, fmt.Errorf("%w: %s: %w", ErrPlacement, node, err)
	}
	if !validID(p.FailureDomain) {
		return Placement{}, fmt.Errorf("%w: node has no independent failure domain", ErrPlacement)
	}
	return p, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func copyChecked(ctx context.Context, into io.Writer, content io.Reader, ref BlobRef) error {
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(into, hash), io.LimitReader(contextReader{ctx, content}, ref.Size+1))
	if err != nil {
		return fmt.Errorf("checkpoint: read content: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if written != ref.Size || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return ErrIntegrity
	}
	return nil
}

func tempName() string {
	var value [16]byte
	// crypto/rand.Read never returns an error: it fills or crashes.
	_, _ = rand.Read(value[:])
	return "tmp/" + hex.EncodeToString(value[:])
}

func (s *Store) checkOpenLocked() error {
	if s.closed {
		return os.ErrClosed
	}
	return nil
}

func (s *Store) openVerifiedLocked(ctx context.Context, scope Scope, ref BlobRef) (*os.File, error) {
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	name := blobName(scope, ref)
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrIncomplete
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Size {
		return nil, ErrIntegrity
	}
	file, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	if err := copyChecked(ctx, io.Discard, file, ref); err != nil {
		// The file was only read; the verification failure is the answer.
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		// Same: nothing was written, the seek failure is the answer.
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// PutBlob accepts only an explicitly described byte stream. It does not read
// a caller's filesystem paths or capture any native session/login state.
func (s *Store) PutBlob(ctx context.Context, scope Scope, ref BlobRef, content io.Reader) error {
	if content == nil {
		return fmt.Errorf("%w: missing content", ErrInvalid)
	}
	if err := validateBlob(ref, s.cfg.Limits); err != nil {
		return err
	}
	if _, err := s.placement(ctx, scope, s.cfg.NodeID); err != nil {
		return err
	}
	name := blobName(scope, ref)
	for {
		s.mu.Lock()
		if err := s.checkOpenLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
		if pending := s.uploads[name]; pending != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-pending:
				continue
			}
		}
		if existing, err := s.openVerifiedLocked(ctx, scope, ref); err == nil {
			err = errors.Join(existing.Close(), s.syncDir(path.Dir(name)), s.syncDir("blobs"))
			s.mu.Unlock()
			if err != nil {
				return err
			}
			return copyChecked(ctx, io.Discard, content, ref)
		} else if !errors.Is(err, ErrIncomplete) && !errors.Is(err, ErrIntegrity) {
			s.mu.Unlock()
			return err
		} else if errors.Is(err, ErrIntegrity) {
			// Repair only regular content, never a symlink. The old bytes
			// remain in place until a full verified replacement is ready.
			info, statErr := s.root.Lstat(name)
			if statErr != nil || !info.Mode().IsRegular() {
				s.mu.Unlock()
				return ErrIntegrity
			}
		}
		_, replacement := s.sizes[name]
		if ref.Size > s.cfg.Limits.MaxBytes-s.used-s.reservedBytes || (!replacement && s.objects+s.reservedObjects >= s.cfg.Limits.MaxObjects) {
			s.mu.Unlock()
			return ErrQuota
		}
		tmp := tempName()
		file, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.reservedBytes += ref.Size
		if !replacement {
			s.reservedObjects++
		}
		s.uploads[name] = make(chan struct{})
		s.mu.Unlock()
		return s.receiveBlob(ctx, scope, ref, content, file, tmp, replacement)
	}
}

// Streaming happens without the store mutex: two nodes can pull each other's
// missing blobs concurrently, and one stalled sender cannot block local reads.
func (s *Store) receiveBlob(ctx context.Context, scope Scope, ref BlobRef, content io.Reader, file *os.File, tmp string, replacement bool) error {
	name := blobName(scope, ref)
	defer func() {
		// On success the file is already closed and the temporary name
		// already renamed away, so both calls report nothing worth
		// hearing; on failure the error being returned is the answer,
		// and a temporary file that survives is swept at the next Open.
		_ = file.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = s.root.Remove(tmp)
		s.reservedBytes -= ref.Size
		if !replacement {
			s.reservedObjects--
		}
		close(s.uploads[name])
		delete(s.uploads, name)
	}()
	if err := copyChecked(ctx, file, content, ref); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if _, err := s.placement(ctx, scope, s.cfg.NodeID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	if err := s.root.MkdirAll(path.Dir(name), 0o700); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, name); err != nil {
		return err
	}
	s.used += ref.Size - s.sizes[name]
	s.sizes[name] = ref.Size
	if !replacement {
		s.objects++
	}
	if err := s.syncDir(path.Dir(name)); err != nil {
		return err
	}
	return s.syncDir("blobs")
}

func (s *Store) HasBlob(ctx context.Context, scope Scope, ref BlobRef) (bool, error) {
	if err := validateBlob(ref, s.cfg.Limits); err != nil {
		return false, err
	}
	if _, err := s.placement(ctx, scope, s.cfg.NodeID); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.openVerifiedLocked(ctx, scope, ref)
	if errors.Is(err, ErrIncomplete) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

func (s *Store) openBlob(ctx context.Context, scope Scope, ref BlobRef) (*os.File, error) {
	if err := validateBlob(ref, s.cfg.Limits); err != nil {
		return nil, err
	}
	if _, err := s.placement(ctx, scope, s.cfg.NodeID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openVerifiedLocked(ctx, scope, ref)
}

func (s *Store) ReadBlob(ctx context.Context, scope Scope, ref BlobRef, into io.Writer) error {
	if into == nil {
		return fmt.Errorf("%w: missing destination", ErrInvalid)
	}
	file, err := s.openBlob(ctx, scope, ref)
	if err != nil {
		return err
	}
	defer file.Close()
	return copyChecked(ctx, into, file, ref)
}

func (s *Store) syncDir(name string) error {
	dir, err := s.root.Open(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) readJSONLocked(name string, into any) (bool, error) {
	if err := s.checkOpenLocked(); err != nil {
		return false, err
	}
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > s.cfg.Limits.MaxManifestBytes {
		return false, ErrIntegrity
	}
	file, err := s.root.Open(name)
	if err != nil {
		return false, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, s.cfg.Limits.MaxManifestBytes+1))
	if err != nil {
		return false, err
	}
	if int64(len(raw)) > s.cfg.Limits.MaxManifestBytes {
		return false, ErrQuota
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return false, fmt.Errorf("%w: invalid stored checkpoint", ErrIntegrity)
	}
	return true, nil
}

func (s *Store) writeJSONLocked(name string, value any) error {
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(raw)) > s.cfg.Limits.MaxManifestBytes {
		return ErrQuota
	}
	var existing json.RawMessage
	if ok, err := s.readJSONLocked(name, &existing); err == nil && ok {
		if bytes.Equal(existing, raw) {
			return s.syncDir(path.Dir(name))
		}
		return fmt.Errorf("%w: immutable checkpoint record changed", ErrIntegrity)
	} else if err != nil {
		return err
	}
	if int64(len(raw)) > s.cfg.Limits.MaxBytes-s.used-s.reservedBytes || s.objects+s.reservedObjects >= s.cfg.Limits.MaxObjects {
		return ErrQuota
	}
	tmp := tempName()
	file, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// On success the temporary name is renamed away; on failure a file that
	// survives is swept at the next Open. Neither result adds to the
	// return value.
	defer s.root.Remove(tmp)
	if _, err := file.Write(raw); err != nil {
		// The write failed and is reported; the close cannot add to it.
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		// The sync failed and is reported; the close cannot add to it.
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, name); err != nil {
		return err
	}
	s.used += int64(len(raw))
	s.objects++
	s.sizes[name] = int64(len(raw))
	return s.syncDir(path.Dir(name))
}

type prepared struct {
	Snapshot Snapshot `json:"snapshot"`
	Receipt  Receipt  `json:"receipt"`
}

// Prepare pins the complete package before acknowledging it. The pin survives
// process restart and GC, including a lost response after the ledger commits.
func (s *Store) Prepare(ctx context.Context, snapshot Snapshot) (Receipt, error) {
	snapshot, id, refs, err := canonical(snapshot, s.cfg.Limits)
	if err != nil {
		return Receipt{}, err
	}
	placement, err := s.placement(ctx, snapshot.Scope, s.cfg.NodeID)
	if err != nil {
		return Receipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ref := range refs {
		file, err := s.openVerifiedLocked(ctx, snapshot.Scope, ref)
		if err != nil {
			return Receipt{}, err
		}
		if err := file.Sync(); err != nil {
			// The sync failed and is reported; the close cannot add to it.
			_ = file.Close()
			return Receipt{}, err
		}
		if err := file.Close(); err != nil {
			return Receipt{}, err
		}
	}
	if err := s.syncDir("blobs/" + scopeID(snapshot.Scope)); err != nil {
		return Receipt{}, err
	}
	if err := s.syncDir("blobs"); err != nil {
		return Receipt{}, err
	}
	var existing prepared
	name := "prepared/" + id + ".json"
	if ok, err := s.readJSONLocked(name, &existing); err != nil {
		return Receipt{}, err
	} else if ok {
		_, storedID, _, err := canonical(existing.Snapshot, s.cfg.Limits)
		if err != nil || storedID != id || existing.Receipt.SnapshotID != id || existing.Receipt.NodeID != s.cfg.NodeID || existing.Receipt.FailureDomain != placement.FailureDomain || existing.Receipt.StoredAt.IsZero() {
			return Receipt{}, ErrIntegrity
		}
		if err := s.syncDir("prepared"); err != nil {
			return Receipt{}, err
		}
		return existing.Receipt, nil
	}
	receipt := Receipt{SnapshotID: id, NodeID: s.cfg.NodeID, FailureDomain: placement.FailureDomain, StoredAt: time.Now().UTC()}
	if err := s.writeJSONLocked(name, prepared{snapshot, receipt}); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
