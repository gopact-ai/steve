package skills

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The skills that ship inside the binary include Anthropic's skill-creator.
// Platform capabilities and their guidance belong to the Steve MCP server.
// Shipped skills are written fresh under the state directory at boot, so an
// upgrade brings new text. The map lists that directory last, so a skill
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
	if err := writeBuiltins(fresh); err != nil {
		// The write error is the answer; what a failed remove leaves
		// behind is cleared by the RemoveAll above at the next boot.
		_ = os.RemoveAll(fresh)
		return "", fmt.Errorf("install built-in skills: %w", err)
	}
	if err := swapBuiltins(root, fresh); err != nil {
		// Same: the swap error is the answer.
		_ = os.RemoveAll(fresh)
		return "", err
	}
	return root, nil
}

// writeBuiltins writes the shipped skills into fresh, scripts executable.
func writeBuiltins(fresh string) error {
	return fs.WalkDir(builtinFS, "builtin", func(path string, d fs.DirEntry, err error) error {
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
}

// swapBuiltins moves fresh into place at root. The previous copy is set
// aside rather than deleted until the swap is through, so a swap that
// fails halfway can put it back and the hub keeps the skills it had.
func swapBuiltins(root, fresh string) error {
	old := root + ".old"
	// A copy set aside by an earlier swap: if it will not go, the rename
	// onto it below reports it.
	_ = os.RemoveAll(old)
	moved := false
	if _, err := os.Stat(root); err == nil {
		if err := os.Rename(root, old); err != nil {
			return err
		}
		moved = true
	}
	if err := os.Rename(fresh, root); err != nil {
		if moved {
			if restoreErr := os.Rename(old, root); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore the previous built-in skills: %w", restoreErr))
			}
		}
		return err
	}
	// The previous copy is only disk now; the next install clears it.
	_ = os.RemoveAll(old)
	return nil
}
