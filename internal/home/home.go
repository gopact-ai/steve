// Package home loads Steve's single-operator identity files.
package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	FileSoul   = "SOUL.md"
	FileUser   = "USER.md"
	FileMemory = "MEMORY.md"

	BudgetSoul   = 8 * 1024
	BudgetUser   = 8 * 1024
	BudgetMemory = 24 * 1024
	BudgetTotal  = 40 * 1024
)

type Mode string

const (
	ModeNone  Mode = ""
	ModeOwner Mode = "owner"
	ModeGuest Mode = "guest"
)

var (
	ErrMissing = errors.New("steve home is missing")
	ErrEscape  = errors.New("steve home path escapes home directory")
)

type Loader interface {
	Load(mode Mode) (Snapshot, error)
}

type Dir struct{ Path string }

func (d Dir) Load(mode Mode) (Snapshot, error) { return Load(d.Path, mode) }

type Snapshot struct {
	Path     string
	Mode     Mode
	Soul     string
	User     string
	Memory   string
	Identity string
	Prompt   string
	Warnings []string
}

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".steve", "home")
	}
	return filepath.Join(home, ".steve", "home")
}

func Bootstrap(path string, ownerOpenID string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create steve home: %w", err)
	}
	user := strings.ReplaceAll(templateUser, templateOwnerID, ownerLabel(ownerOpenID))
	files := []struct {
		name string
		body string
	}{
		{FileSoul, templateSoul},
		{FileUser, user},
		{FileMemory, templateMemory},
	}
	for _, file := range files {
		if err := writeMissing(filepath.Join(path, file.name), file.body); err != nil {
			return err
		}
	}
	return nil
}

func ownerLabel(ownerOpenID string) string {
	if strings.TrimSpace(ownerOpenID) == "" {
		return unsetOwnerOpenID
	}
	return ownerOpenID
}

func writeMissing(path, body string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := file.WriteString(body); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

func Load(path string, mode Mode) (Snapshot, error) {
	if mode == ModeNone {
		return Snapshot{Mode: ModeNone}, nil
	}
	resolvedHome, err := resolveHome(path)
	if err != nil {
		return Snapshot{}, err
	}
	soul, err := readHomeFile(resolvedHome, FileSoul)
	if err != nil {
		return Snapshot{}, err
	}
	user, err := readHomeFile(resolvedHome, FileUser)
	if err != nil {
		return Snapshot{}, err
	}
	memory, err := readHomeFile(resolvedHome, FileMemory)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Path: resolvedHome, Mode: mode, Soul: soul, User: user, Memory: memory}
	if mode != ModeOwner {
		snap.User = ""
		snap.Memory = ""
	}
	snap.Identity, snap.Prompt, snap.Warnings = compose(resolvedHome, mode, soul, snap.User, snap.Memory)
	return snap, nil
}

func resolveHome(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("steve home: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrMissing
		}
		return "", fmt.Errorf("stat steve home: %w", err)
	}
	if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("steve home is not a directory")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrMissing
		}
		return "", fmt.Errorf("resolve steve home: %w", err)
	}
	info, err = os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrMissing
		}
		return "", fmt.Errorf("stat steve home: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("steve home is not a directory")
	}
	return resolved, nil
}

func readHomeFile(resolvedHome, name string) (string, error) {
	path := filepath.Join(resolvedHome, name)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrMissing
		}
		return "", fmt.Errorf("stat %s: %w", name, err)
	}
	target := path
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", ErrMissing
			}
			return "", fmt.Errorf("resolve %s: %w", name, err)
		}
		if err := withinHome(resolvedHome, resolved); err != nil {
			return "", err
		}
		target = resolved
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return string(data), nil
}

func withinHome(resolvedHome, resolvedFile string) error {
	rel, err := filepath.Rel(resolvedHome, resolvedFile)
	if err != nil {
		return ErrEscape
	}
	if rel == "." || rel == "" {
		return ErrEscape
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return ErrEscape
	}
	return nil
}

func compose(path string, mode Mode, soul, user, memory string) (identity, prompt string, warnings []string) {
	soul = truncateTo(soul, BudgetSoul)
	user = truncateTo(user, BudgetUser)
	memory = truncateTo(memory, BudgetMemory)

	var parts []string
	if mode == ModeOwner {
		parts = append(parts, ownerWrapper(path))
	} else {
		parts = append(parts, guestWrapper())
	}
	if soul != "" {
		parts = append(parts, soul)
	}
	if mode == ModeOwner && user != "" {
		parts = append(parts, user)
	}
	core := strings.Join(parts, "\n\n")
	if mode == ModeOwner && memory != "" {
		memory = fitRemainder(core, memory)
	} else {
		memory = ""
	}
	if mode == ModeOwner && user != "" && overBudget(joinNonEmpty(core, memory), BudgetTotal) {
		user = fitRemainder(joinNonEmpty(partsWithoutLast(parts)...), user)
		parts = append([]string{}, parts[:len(parts)-1]...)
		if user != "" {
			parts = append(parts, user)
		}
		core = strings.Join(parts, "\n\n")
		if memory != "" {
			memory = fitRemainder(core, memory)
		}
	}
	identity = core
	prompt = joinNonEmpty(core, memory)
	warnings = budgetWarnings(soul, user, memory)
	return identity, prompt, warnings
}

func partsWithoutLast(parts []string) []string {
	if len(parts) == 0 {
		return nil
	}
	return parts[:len(parts)-1]
}

func joinNonEmpty(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n\n")
}

func overBudget(s string, budget int) bool {
	return len([]byte(s)) > budget
}

func fitRemainder(prefix, extra string) string {
	if extra == "" {
		return ""
	}
	sep := 0
	if prefix != "" {
		sep = len("\n\n")
	}
	room := BudgetTotal - len([]byte(prefix)) - sep
	if room <= 0 {
		return ""
	}
	return truncateTo(extra, room)
}

func truncateTo(s string, budget int) string {
	if budget <= 0 || s == "" {
		return ""
	}
	if len([]byte(s)) <= budget {
		return s
	}
	reserve := len([]byte(TruncationMarker))
	if budget < reserve {
		return ""
	}
	body := []byte(s)
	maxBody := budget - reserve
	if len(body) > maxBody {
		body = body[:maxBody]
	}
	for !utf8.ValidString(string(body)) && len(body) > 0 {
		body = body[:len(body)-1]
	}
	return string(body) + TruncationMarker
}

func budgetWarnings(soul, user, memory string) []string {
	var warnings []string
	for _, item := range []struct {
		name   string
		text   string
		budget int
	}{
		{FileSoul, soul, BudgetSoul},
		{FileUser, user, BudgetUser},
		{FileMemory, memory, BudgetMemory},
	} {
		if item.text == "" {
			continue
		}
		if len([]byte(item.text))*5 >= item.budget*4 {
			warnings = append(warnings, fmt.Sprintf("%s is over 80%% of its budget", item.name))
		}
	}
	return warnings
}
