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
	"sort"
	"strings"
)

// A machine's own skills are the ones its AI tools were given outside
// Steve: directories with a SKILL.md under the tools' home directories.
// The hub asks each machine for the list with a shell script — the exec
// stream is what every machine has — and, when the owner loads one,
// for its files as a tar stream, which land in the owner's own skills
// directory on the hub. From there it is a hub skill like any other.

// Found is one skill on a machine, as the scan reports it.
type Found struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// machineDirs are where the AI tools keep skills of their own. Steve's
// isolated runtime homes are not among them: what is there came from
// the hub.
var machineDirs = []string{"$HOME/.codex/skills", "$HOME/.claude/skills", "$HOME/.grok/skills", "$HOME/.kimi/skills", "$HOME/.agents/skills"}

const (
	scanBegin = "=== steve-skill "
	scanEnd   = "=== steve-skill-end"
	importCap = 16 << 20
)

// ScanScript lists the skills on a machine: for each, its directory and
// the head of its SKILL.md, framed so the hub can read them back.
func ScanScript() string {
	// The directory is printed as its physical path: a tool that links
	// its skills directory to another's (~/.agents/skills is often
	// ~/.codex/skills) would otherwise list every skill twice.
	return strings.Join([]string{
		`for d in ` + strings.Join(machineDirs, " ") + `; do`,
		`  [ -d "$d" ] || continue`,
		`  for s in "$d"/*; do`,
		`    [ -f "$s/SKILL.md" ] || continue`,
		`    real=$(cd "$s" 2>/dev/null && pwd -P) || continue`,
		`    printf '` + scanBegin + `%s\n' "$real"`,
		`    head -c 6000 "$s/SKILL.md"`,
		`    printf '\n` + scanEnd + `\n'`,
		`  done`,
		`done 2>/dev/null`,
	}, "\n")
}

// ParseScan reads what ScanScript printed.
func ParseScan(output string) []Found {
	var out []Found
	seen := map[string]bool{}
	rest := output
	for {
		i := strings.Index(rest, scanBegin)
		if i < 0 {
			break
		}
		rest = rest[i+len(scanBegin):]
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		path := strings.TrimSpace(rest[:nl])
		rest = rest[nl+1:]
		end := strings.Index(rest, scanEnd)
		if end < 0 {
			break
		}
		body := rest[:end]
		rest = rest[end+len(scanEnd):]
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		d := DescribeText(strings.NewReader(body))
		out = append(out, Found{Name: filepath.Base(path), Path: path, Title: d.Title, Description: d.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

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
