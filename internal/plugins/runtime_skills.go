package plugins

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MaterializeRuntimeSkills copies immutable package files under names derived
// from their full capability identities. A skill never points at a mutable
// source checkout or a machine's current global skills directory.
func (s *Store) MaterializeRuntimeSkills(ctx context.Context, selection Selection, dir string) (string, error) {
	receipts, err := s.Selection(selection)
	if err != nil {
		return "", err
	}
	files := map[string]packageFile{}
	for _, receipt := range receipts {
		bundle, err := s.Read(receipt.Deployment.Digest)
		if err != nil {
			return "", err
		}
		packed, err := archiveFiles(bundle.Data)
		if err != nil {
			return "", err
		}
		for _, name := range sortedKeys(bundle.Manifest.Skills) {
			native, err := NativeName(bundle.Manifest.ID, "skill", name)
			if err != nil {
				return "", err
			}
			prefix := bundle.Manifest.Skills[name] + "/"
			for _, file := range sortedKeys(packed) {
				if strings.HasPrefix(file, prefix) {
					destination := path.Join(native, strings.TrimPrefix(file, prefix))
					if _, exists := files[destination]; exists {
						return "", fmt.Errorf("%w: repeated runtime skill", ErrConflict)
					}
					files[destination] = packed[file]
				}
			}
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	for _, name := range sortedKeys(files) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		file := files[name]
		dest := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return "", err
		}
		mode := os.FileMode(0600)
		if file.executable {
			mode = 0700
		}
		if err := writeSynced(dest, file.data, mode); err != nil {
			return "", err
		}
	}
	return runtimeSkillsHash(files), nil
}

func runtimeSkillsHash(files map[string]packageFile) string {
	var identity strings.Builder
	for _, name := range sortedKeys(files) {
		file := files[name]
		fmt.Fprintf(&identity, "%s\x00%t\x00%s\n", name, file.executable, contentDigest(file.data))
	}
	return contentDigest([]byte(identity.String()))
}

func (s *Store) RuntimeInstructions(selection Selection) (string, error) {
	receipts, err := s.Selection(selection)
	if err != nil {
		return "", err
	}
	var instructions []string
	for _, receipt := range receipts {
		bundle, err := s.Read(receipt.Deployment.Digest)
		if err != nil {
			return "", err
		}
		files, err := archiveFiles(bundle.Data)
		if err != nil {
			return "", err
		}
		for _, name := range sortedKeys(bundle.Manifest.Skills) {
			instructions = append(instructions, string(files[path.Join(bundle.Manifest.Skills[name], "SKILL.md")].data))
		}
	}
	text := strings.Join(instructions, "\n\n")
	if len(text) > MaxPackageBytes {
		return "", ErrInvalid
	}
	return text, nil
}

func SkillsDigest(ctx context.Context, dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	files := map[string]packageFile{}
	total := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !packagePath(name) {
			return ErrInvalid
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrIntegrity
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		total += len(raw)
		if len(raw) > MaxFileBytes || total > MaxPackageBytes || len(files) >= MaxFiles {
			return ErrInvalid
		}
		files[name] = packageFile{data: raw, executable: info.Mode()&0111 != 0}
		return nil
	})
	if err != nil {
		return "", err
	}
	return runtimeSkillsHash(files), nil
}
