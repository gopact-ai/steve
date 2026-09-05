package skills

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one skill in a bundle: its name and the hash of its content.
type Entry struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// Bundle is the enabled skills packed for shipping to a machine: a
// deterministic tar, addressed by its own hash, so two hubs packing the
// same skills produce the same bytes and a node can tell "already have it"
// from the name alone.
type Bundle struct {
	Hash   string
	Skills []Entry
	Data   []byte
}

// MaxBundleBytes bounds a bundle. Skills are text; something larger is a
// mistake, not a skill.
const MaxBundleBytes = 32 << 20

// Pack packs the skill directories into a bundle. Only regular files go
// in; symlinks and special files are skipped. Headers carry no times,
// owners or modes beyond the executable bit, so the bytes depend on the
// content alone.
func Pack(refs []Ref) (Bundle, error) {
	sorted := append([]Ref(nil), refs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var entries []Entry
	seen := map[string]bool{}
	for _, ref := range sorted {
		if ref.Name == "" || ref.Name != filepath.Base(ref.Name) || strings.HasPrefix(ref.Name, ".") {
			return Bundle{}, fmt.Errorf("skill name %q is not a plain directory name", ref.Name)
		}
		if seen[ref.Name] {
			return Bundle{}, fmt.Errorf("skill %q listed twice", ref.Name)
		}
		seen[ref.Name] = true
		files, err := listFiles(ref.Path)
		if err != nil {
			return Bundle{}, fmt.Errorf("skill %q: %w", ref.Name, err)
		}
		h := sha256.New()
		for _, rel := range files {
			full := filepath.Join(ref.Path, filepath.FromSlash(rel))
			info, err := os.Stat(full)
			if err != nil {
				return Bundle{}, err
			}
			content, err := os.ReadFile(full)
			if err != nil {
				return Bundle{}, err
			}
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			name := ref.Name + "/" + rel
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}); err != nil {
				return Bundle{}, err
			}
			if _, err := tw.Write(content); err != nil {
				return Bundle{}, err
			}
			fmt.Fprintf(h, "%s\x00%d\x00", rel, mode)
			h.Write(content)
			h.Write([]byte{0})
			if buf.Len() > MaxBundleBytes {
				return Bundle{}, fmt.Errorf("skills exceed %d bytes", MaxBundleBytes)
			}
		}
		entries = append(entries, Entry{Name: ref.Name, Hash: hex.EncodeToString(h.Sum(nil))})
	}
	if err := tw.Close(); err != nil {
		return Bundle{}, err
	}
	sum := sha256.Sum256(buf.Bytes())
	return Bundle{Hash: hex.EncodeToString(sum[:]), Skills: entries, Data: buf.Bytes()}, nil
}

// HashOf is the hash a bundle with these bytes has; a receiver checks it
// before trusting the name it arrived under.
func HashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Unpack extracts a bundle into dir, one subdirectory per skill, and
// returns the entries with their content hashes computed the way Pack
// computes them. Paths that would leave dir, and anything that is not a
// regular file, are refused: the bundle came over the network.
func Unpack(data []byte, dir string) ([]Entry, error) {
	if len(data) > MaxBundleBytes {
		return nil, fmt.Errorf("bundle exceeds %d bytes", MaxBundleBytes)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tr := tar.NewReader(bytes.NewReader(data))
	names := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read bundle: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("bundle entry %q is not a regular file", hdr.Name)
		}
		clean := path.Clean(hdr.Name)
		if path.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
			return nil, fmt.Errorf("bundle entry %q escapes the bundle", hdr.Name)
		}
		skill, _, ok := strings.Cut(clean, "/")
		if !ok || skill == "" || strings.HasPrefix(skill, ".") {
			return nil, fmt.Errorf("bundle entry %q is not inside a skill", hdr.Name)
		}
		names[skill] = true
		full := filepath.Join(dir, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return nil, err
		}
		mode := fs.FileMode(0o600)
		if hdr.Mode&0o111 != 0 {
			mode = 0o700
		}
		file, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(file, io.LimitReader(tr, MaxBundleBytes)); err != nil {
			file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	entries := make([]Entry, 0, len(names))
	for name := range names {
		hash, err := HashDir(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{Name: name, Hash: hash})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// HashDir is the content hash of one skill directory, as Pack computes it.
func HashDir(root string) (string, error) {
	files, err := listFiles(root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, rel := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(full)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return "", err
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, mode)
		h.Write(content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// listFiles is every regular file under root, as sorted slash paths.
func listFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
