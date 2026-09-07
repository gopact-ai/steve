package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// Restore materializes selected portable files into an exclusively created
// directory. It never writes to a project's canonical directory. Consumers
// must wait for success and acquire fresh execution authority before running.
func (s *Store) Restore(ctx context.Context, manifest Manifest, directory string) (Restored, error) {
	verified, err := s.Verify(ctx, manifest)
	if err != nil {
		return Restored{}, err
	}
	if directory == "" {
		return Restored{}, fmt.Errorf("%w: a new destination directory is required", ErrInvalid)
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return Restored{}, err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return Restored{}, fmt.Errorf("checkpoint: create unused recovery directory: %w", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		_ = os.Remove(directory)
		return Restored{}, err
	}
	defer root.Close()
	complete := false
	defer func() {
		if !complete {
			_ = root.RemoveAll("workspace")
			_ = root.RemoveAll("materials")
			_ = root.Remove("context.json")
			_ = os.Remove(directory)
		}
	}()
	m := verified.Manifest()
	write := func(name string, ref BlobRef, executable bool) error {
		if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if executable {
			mode = 0o700
		}
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		if err := s.ReadBlob(ctx, m.Snapshot.Scope, ref, file); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}
	if err := write("context.json", m.Snapshot.Context, false); err != nil {
		return Restored{}, err
	}
	for _, group := range []struct {
		name  string
		files []File
	}{{"workspace", m.Snapshot.Workspace.Files}, {"materials", m.Snapshot.Materials}} {
		if err := root.MkdirAll(group.name, 0o700); err != nil {
			return Restored{}, err
		}
		for _, file := range group.files {
			if err := write(group.name+"/"+file.Path, file.Blob, file.Executable); err != nil {
				return Restored{}, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Restored{}, err
	}
	complete = true
	return Restored{Directory: directory, Context: filepath.Join(directory, "context.json"), Workspace: filepath.Join(directory, "workspace"), Materials: filepath.Join(directory, "materials")}, nil
}

// Load returns only the local index of ledger-committed checkpoints. Prepared
// replica receipts are not exposed as committed recovery state.
func (s *Store) Load(ctx context.Context, id string) (Manifest, bool, error) {
	if !validDigest(id) {
		return Manifest{}, false, fmt.Errorf("%w: invalid manifest ID", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, false, err
	}
	s.mu.Lock()
	var m Manifest
	ok, err := s.readJSONLocked("manifests/"+id+".json", &m)
	s.mu.Unlock()
	if err != nil || !ok {
		return Manifest{}, false, err
	}
	validated, err := validateManifest(m, s.cfg.Limits)
	if err != nil {
		return Manifest{}, false, err
	}
	if validated.ID != id {
		return Manifest{}, false, ErrIntegrity
	}
	if _, err := s.placement(ctx, validated.Snapshot.Scope, s.cfg.NodeID); err != nil {
		return Manifest{}, false, err
	}
	return validated, true, nil
}

// ReadContext returns the explicitly supplied portable context, with an
// additional caller bound for consumers that need a small prompt payload.
func (s *Store) ReadContext(ctx context.Context, manifest Manifest, maxBytes int64) ([]byte, error) {
	verified, err := s.Verify(ctx, manifest)
	if err != nil {
		return nil, err
	}
	m := verified.Manifest()
	if maxBytes < 1 || m.Snapshot.Context.Size > maxBytes {
		return nil, ErrQuota
	}
	var content bytes.Buffer
	if err := s.ReadBlob(ctx, m.Snapshot.Scope, m.Snapshot.Context, &content); err != nil {
		return nil, err
	}
	return content.Bytes(), nil
}
