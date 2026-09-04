package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Loading a machine's skill onto the hub: the machine sends the
// directory as a tar stream over the exec stream (what every machine
// has), and the hub unpacks it into the owner's own skills directory.
// From there it is a hub skill like any other.

// Found is one skill on a machine, as the scan reports it.
type Found struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

const importCap = 16 << 20

// ImportScript sends a skill's files as a base64 tar stream.
func ImportScript(path string) string {
	return "cd " + shellQuote(path) + " && tar czf - --exclude=.git --exclude=__pycache__ --exclude=node_modules . | base64"
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// UnpackImport writes what ImportScript sent into dest, which must not exist
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
