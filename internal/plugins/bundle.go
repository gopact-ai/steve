package plugins

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

type Bundle struct {
	Manifest Manifest `json:"manifest"`
	Digest   string   `json:"digest"`
	Data     []byte   `json:"-"`
}

type packageFile struct {
	data       []byte
	executable bool
}

// ReadDirectory snapshots regular files through a confined root. Source
// symlinks are refused, even when they would stay inside the package.
func ReadDirectory(ctx context.Context, directory string) (Bundle, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return Bundle{}, err
	}
	if !info.IsDir() {
		return Bundle{}, fmt.Errorf("%w: source must be a directory, not a link", ErrInvalid)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return Bundle{}, err
	}
	defer root.Close()
	return readRoot(ctx, root, true)
}

func readRoot(ctx context.Context, root *os.Root, source bool) (Bundle, error) {
	files := map[string]packageFile{}
	total, entries := 0, 0
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if source && name == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		entries++
		if entries > MaxFiles {
			return fmt.Errorf("%w: too many package entries", ErrInvalid)
		}
		if !packagePath(name) {
			return fmt.Errorf("%w: invalid package path %q", ErrInvalid, name)
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: package entry %q is not a regular file", ErrInvalid, name)
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		current, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return statErr
		}
		if !current.Mode().IsRegular() || !os.SameFile(info, current) {
			f.Close()
			return fmt.Errorf("%w: package entry changed while reading %q", ErrInvalid, name)
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		total += len(raw)
		if len(raw) > MaxFileBytes || total > MaxPackageBytes {
			return fmt.Errorf("%w: package size limit", ErrInvalid)
		}
		files[name] = packageFile{data: raw, executable: info.Mode()&0111 != 0}
		return nil
	})
	if err != nil {
		return Bundle{}, err
	}
	return packFiles(files)
}

func packFiles(files map[string]packageFile) (Bundle, error) {
	m, err := ParseManifest(files[ManifestFile].data)
	if err != nil {
		return Bundle{}, err
	}
	if err := validateEntries(m, files); err != nil {
		return Bundle{}, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return Bundle{}, err
	}
	files[ManifestFile] = packageFile{data: raw}
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, name := range sortedKeys(files) {
		file := files[name]
		mode := int64(0644)
		if file.executable {
			mode = 0755
		}
		header := &tar.Header{Name: name, Mode: mode, Size: int64(len(file.data)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
		if err := writer.WriteHeader(header); err != nil {
			return Bundle{}, err
		}
		if _, err := writer.Write(file.data); err != nil {
			return Bundle{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return Bundle{}, err
	}
	if buf.Len() > MaxPackageBytes {
		return Bundle{}, fmt.Errorf("%w: archive size limit", ErrInvalid)
	}
	data := buf.Bytes()
	return Bundle{Manifest: m, Digest: contentDigest(data), Data: data}, nil
}

func validateEntries(m Manifest, files map[string]packageFile) error {
	directories := map[string]bool{}
	portable := map[string]string{}
	for _, name := range sortedKeys(files) {
		if err := claimPortablePath(portable, name); err != nil {
			return err
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := files[parent]; exists {
				return fmt.Errorf("%w: file is also a directory", ErrInvalid)
			}
			if err := claimPortablePath(portable, parent); err != nil {
				return err
			}
			directories[parent] = true
		}
	}
	if len(files)+len(directories) > MaxFiles {
		return fmt.Errorf("%w: too many package entries", ErrInvalid)
	}

	for _, name := range sortedKeys(m.Skills) {
		if _, ok := files[path.Join(m.Skills[name], "SKILL.md")]; !ok {
			return fmt.Errorf("%w: skill %s has no SKILL.md", ErrInvalid, name)
		}
	}
	for _, name := range sortedKeys(m.MCP) {
		program := m.MCP[name].Program
		if program == nil {
			continue
		}
		file, ok := files[program.Path]
		if !ok || program.Runtime == "" && !file.executable {
			return fmt.Errorf("%w: MCP %s package program missing or not executable", ErrInvalid, name)
		}
	}
	return nil
}

func contentDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// DecodeBundle accepts only the canonical archive produced by the snapshotter.
// It validates all entries before any of them are written to disk.
func DecodeBundle(data []byte) (Bundle, error) {
	files, err := archiveFiles(data)
	if err != nil {
		return Bundle{}, err
	}
	canonical, err := packFiles(files)
	if err != nil {
		return Bundle{}, err
	}
	if !bytes.Equal(canonical.Data, data) {
		return Bundle{}, fmt.Errorf("%w: archive is not canonical", ErrIntegrity)
	}
	return canonical, nil
}

func archiveFiles(data []byte) (map[string]packageFile, error) {
	if len(data) > MaxPackageBytes {
		return nil, fmt.Errorf("%w: archive size limit", ErrInvalid)
	}
	reader := tar.NewReader(bytes.NewReader(data))
	files := map[string]packageFile{}
	total := int64(0)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if !packagePath(header.Name) || header.Typeflag != tar.TypeReg || header.Mode != 0644 && header.Mode != 0755 {
			return nil, fmt.Errorf("%w: invalid archive entry %q", ErrInvalid, header.Name)
		}
		if _, exists := files[header.Name]; exists {
			return nil, fmt.Errorf("%w: repeated archive entry", ErrInvalid)
		}
		total += header.Size
		if header.Size < 0 || header.Size > MaxFileBytes || total > MaxPackageBytes || len(files) >= MaxFiles {
			return nil, fmt.Errorf("%w: archive entry limit", ErrInvalid)
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		files[header.Name] = packageFile{data: raw, executable: header.Mode == 0755}
	}
	return files, nil
}

func claimPortablePath(paths map[string]string, name string) error {
	key := strings.ToLower(norm.NFC.String(name))
	if previous, exists := paths[key]; exists && previous != name {
		return fmt.Errorf("%w: paths %q and %q collide on a case-insensitive filesystem", ErrInvalid, previous, name)
	}
	paths[key] = name
	return nil
}
