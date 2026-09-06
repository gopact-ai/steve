package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Loading a machine's skill onto the hub: the machine sends the
// directory as a tar stream through a typed file request, and the hub
// unpacks it into the owner's own skills directory.
// From there it is a hub skill like any other.

// Found is one skill on a machine, as the scan reports it.
type Found struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

const importCap = 16 << 20

// PackImport encodes a skill using only Go. Like the import side, it skips
// links and devices, and excludes generated directories at every depth.
func PackImport(ctx context.Context, dir string) (string, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	var size int64
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path != dir {
			switch entry.Name() {
			case ".git", "__pycache__", "node_modules":
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		if size > importCap || buf.Len() > importCap {
			return fmt.Errorf("skill exceeds the %d MB import cap", importCap>>20)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.CopyN(tw, file, info.Size())
		return err
	})
	if err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if buf.Len() > importCap {
		return "", fmt.Errorf("skill exceeds the %d MB import cap", importCap>>20)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// UnpackImport writes what PackImport sent into dest, which must not exist
// yet. Every entry stays under dest; the stream is capped.
func UnpackImport(encoded, dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	if err != nil {
		return fmt.Errorf("the machine did not send a tar stream: %w", err)
	}
	if len(raw) > importCap {
		return fmt.Errorf("skill is %d MB; the cap is %d MB", len(raw)>>20, importCap>>20)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer gz.Close()
	tmp := dest + ".loading"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		name := filepath.Clean("/" + h.Name)
		target := filepath.Join(tmp, name)
		if !strings.HasPrefix(target, tmp+string(filepath.Separator)) && target != tmp {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("entry %q escapes the skill", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				_ = os.RemoveAll(tmp)
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				_ = os.RemoveAll(tmp)
				return err
			}
			mode := os.FileMode(0o600)
			if h.Mode&0o111 != 0 {
				mode = 0o700
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				_ = os.RemoveAll(tmp)
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, importCap)); err != nil {
				f.Close()
				_ = os.RemoveAll(tmp)
				return err
			}
			f.Close()
		default:
			// Links and devices are not part of a skill.
		}
	}
	if !hasSkill(tmp) {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("no SKILL.md in what the machine sent")
	}
	return os.Rename(tmp, dest)
}
