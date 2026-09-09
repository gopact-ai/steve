package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

func readRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: expected a regular file", ErrIntegrity)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w: stored file too large", ErrIntegrity)
	}
	return raw, nil
}

func (s *Store) readReceipt(id string) (Receipt, bool, error) {
	if !nameShape.MatchString(id) {
		return Receipt{}, false, fmt.Errorf("%w: receipt ID", ErrInvalid)
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return Receipt{}, false, err
	}
	defer root.Close()
	raw, err := readRegular(root, "receipts/"+id+".json", MaxManifestBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	var receipt Receipt
	if err := decodeStrict(raw, &receipt); err != nil {
		return receipt, false, err
	}
	if receipt.Schema != Schema || receipt.CommandID != id || !ValidID(receipt.ID) || !validVersion(receipt.Version) || !digestShape.MatchString(receipt.Digest) || receipt.State != Prepared || receipt.PreparedAt.IsZero() {
		return receipt, false, fmt.Errorf("%w: invalid receipt", ErrIntegrity)
	}
	if err := receipt.Source.validate(); err != nil {
		return receipt, false, err
	}
	pending, found, err := s.readPreparation(id)
	if err != nil {
		return receipt, false, err
	}
	if !found || pending.Digest != receipt.Digest || pending.ID != receipt.ID || pending.Version != receipt.Version || pending.Source != receipt.Source {
		return receipt, false, fmt.Errorf("%w: receipt differs from preparation", ErrIntegrity)
	}
	return receipt, true, nil
}

func (s *Store) publishBundle(ctx context.Context, bundle Bundle) error {
	dest := filepath.Join(s.Dir, "packages", bundle.Digest)
	if _, err := os.Lstat(dest); err == nil {
		if _, err := s.Read(bundle.Digest); err != nil {
			return err
		}
		return s.sync(filepath.Dir(dest))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Join(s.Dir, "packages"), ".prepare-")
	if err != nil {
		return err
	}
	defer removeStaging(staging)
	content := filepath.Join(staging, "content")
	if err := os.Mkdir(content, 0700); err != nil {
		return err
	}
	files, err := archiveFiles(bundle.Data)
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(files) {
		if err := ctx.Err(); err != nil {
			return err
		}
		file := files[name]
		dest := filepath.Join(content, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		mode := fs.FileMode(0600)
		if file.executable {
			mode = 0700
		}
		if err := writeSynced(dest, file.data, mode); err != nil {
			return err
		}
	}
	if err := writeSynced(filepath.Join(staging, "bundle.tar"), bundle.Data, 0600); err != nil {
		return err
	}
	if err := s.syncTree(staging); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(staging, dest); err != nil {
		return err
	}
	return s.sync(filepath.Dir(dest))
}

func writeSynced(path string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	return errors.Join(writeErr, f.Close())
}

func (s *Store) syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Reverse(dirs)
	for _, dir := range dirs {
		if err := s.sync(dir); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeReceipt(receipt Receipt) error {
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	return s.writeRecord(filepath.Join(s.Dir, "receipts", receipt.CommandID+".json"), raw)
}

func (s *Store) writeRecord(path string, raw []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".record-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer removeStaging(name)
	_, err = temp.Write(append(raw, '\n'))
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return s.sync(dir)
}

func removeStaging(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("plugins: preparation staging could not be removed", "path", path, "error", err)
	}
}
