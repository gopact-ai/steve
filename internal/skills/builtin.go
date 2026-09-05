package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The skills that ship inside the binary: Anthropic's skill-creator, and
// the steve suite that tells an agent how this system works. They are
// written under the state directory at boot, fresh each time, so an
// upgrade brings new text; the map lists that directory last, so a skill
// of the same name in the user's own directories wins.
//
//go:embed all:builtin
var builtinFS embed.FS

// BuiltinRoot is where the shipped skills live under the state directory.
func BuiltinRoot(stateDir string) string { return filepath.Join(stateDir, "skills-builtin") }

// BuiltinNames lists the shipped skills.
func BuiltinNames() ([]string, error) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := fs.Stat(builtinFS, "builtin/"+e.Name()+"/SKILL.md"); err == nil {
				names = append(names, e.Name())
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// InstallBuiltins writes the shipped skills under the state directory and
// returns where. It writes beside the old copy and swaps, so a harness
// reading a skill mid-boot sees the old text or the new, never a torn
// directory; a skill that shipped before and does not now is gone.
func InstallBuiltins(stateDir string) (string, error) {
	root := BuiltinRoot(stateDir)
	fresh := root + ".new"
	if err := os.RemoveAll(fresh); err != nil {
		return "", err
	}
	err := fs.WalkDir(builtinFS, "builtin", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "builtin")
		rel = strings.TrimPrefix(rel, "/")
		target := filepath.Join(fresh, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := builtinFS.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if strings.HasSuffix(path, ".py") || strings.HasSuffix(path, ".sh") {
			mode = 0o700
		}
		return os.WriteFile(target, data, mode)
	})
	if err != nil {
		_ = os.RemoveAll(fresh)
		return "", fmt.Errorf("install built-in skills: %w", err)
	}
	old := root + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(root); err == nil {
		if err := os.Rename(root, old); err != nil {
			_ = os.RemoveAll(fresh)
			return "", err
		}
	}
	if err := os.Rename(fresh, root); err != nil {
		_ = os.Rename(old, root)
		_ = os.RemoveAll(fresh)
		return "", err
	}
	_ = os.RemoveAll(old)
	return root, nil
}
