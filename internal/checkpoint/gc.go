package checkpoint

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
)

// GC removes only unreferenced uploaded blobs. Both prepared packages and
// committed manifests retain their entire closure; an interrupted ledger
// response cannot cause data loss. Retiring checkpoints is a ledger retention
// decision and deliberately is not inferred from age or node liveness here.
func (s *Store) GC(ctx context.Context) (GCResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return GCResult{}, err
	}
	keep := map[string]bool{}
	for name := range s.pins {
		keep[name] = true
	}
	for name := range s.uploads {
		keep[name] = true
	}
	for _, directory := range []string{"prepared", "manifests"} {
		entries, err := fs.ReadDir(s.root.FS(), directory)
		if err != nil {
			return GCResult{}, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return GCResult{}, err
			}
			name := directory + "/" + entry.Name()
			var snapshot Snapshot
			if directory == "prepared" {
				var pending prepared
				if _, err := s.readJSONLocked(name, &pending); err != nil {
					return GCResult{}, err
				}
				_, id, _, err := canonical(pending.Snapshot, s.cfg.Limits)
				if err != nil || entry.Name() != id+".json" || pending.Receipt.SnapshotID != id {
					return GCResult{}, ErrIntegrity
				}
				snapshot = pending.Snapshot
			} else {
				var m Manifest
				if _, err := s.readJSONLocked(name, &m); err != nil {
					return GCResult{}, err
				}
				validated, err := validateManifest(m, s.cfg.Limits)
				if err != nil || entry.Name() != validated.ID+".json" {
					return GCResult{}, ErrIntegrity
				}
				snapshot = validated.Snapshot
			}
			_, _, refs, err := canonical(snapshot, s.cfg.Limits)
			if err != nil {
				return GCResult{}, err
			}
			for _, ref := range refs {
				keep[blobName(snapshot.Scope, ref)] = true
			}
		}
	}
	var remove []string
	err := fs.WalkDir(s.root.FS(), "blobs", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		parts := strings.Split(name, "/")
		if len(parts) != 3 || !validDigest(parts[1]) || !validDigest(parts[2]) || !entry.Type().IsRegular() {
			return fmt.Errorf("%w: invalid content-store entry", ErrIntegrity)
		}
		if !keep[name] {
			remove = append(remove, name)
		}
		return nil
	})
	if err != nil {
		return GCResult{}, err
	}
	var result GCResult
	for _, name := range remove {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		info, err := s.root.Lstat(name)
		if err != nil {
			return result, err
		}
		if err := s.root.Remove(name); err != nil {
			return result, err
		}
		s.used -= s.sizes[name]
		delete(s.sizes, name)
		s.objects--
		result.Bytes += info.Size()
		result.Blobs++
		parent, _, _ := strings.Cut(name[6:], "/")
		if err := s.syncDir("blobs/" + parent); err != nil {
			return result, err
		}
		// An unreferenced scope must not leave an unlimited number of empty
		// directories after repeated zero-byte uploads and collection.
		entries, err := fs.ReadDir(s.root.FS(), "blobs/"+parent)
		if err != nil {
			return result, err
		}
		if len(entries) == 0 {
			if err := s.root.Remove("blobs/" + parent); err != nil {
				return result, err
			}
			if err := s.syncDir("blobs"); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}
