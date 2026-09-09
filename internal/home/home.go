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

type Dir struct {
	Path   string
	Locale Locale
}

func (d Dir) Load(mode Mode) (Snapshot, error) {
	return LoadWithLocale(d.Path, mode, d.Locale)
}

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
	return BootstrapLocale(path, ownerOpenID, LocaleZH)
}

func BootstrapLocale(path, ownerOpenID string, locale Locale) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create steve home: %w", err)
	}
	pack := templatesFor(locale)
	user := pack.user
	files := []struct {
		name string
		body string
	}{
		{FileSoul, pack.soul},
		{FileUser, user},
		{FileMemory, pack.memory},
	}
	for _, file := range files {
		if err := writeMissing(filepath.Join(path, file.name), file.body); err != nil {
			return err
		}
	}
	return nil
}

func IsTemplate(body string) bool {
	return strings.Contains(body, TemplateMarker)
}

// NeedsInit reports whether SOUL.md or USER.md is missing or still a template.
func NeedsInit(path string) bool {
	for _, name := range []string{FileSoul, FileUser} {
		body, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || IsTemplate(string(body)) {
			return true
		}
	}
	return false
}

func WriteIdentity(path, soul, user string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create steve home: %w", err)
	}
	soul = stripTemplateMarker(soul)
	user = stripTemplateMarker(user)
	if strings.TrimSpace(soul) == "" || strings.TrimSpace(user) == "" {
		return fmt.Errorf("steve home identity is empty")
	}
	if err := writeReplace(filepath.Join(path, FileSoul), soul); err != nil {
		return err
	}
	return writeReplace(filepath.Join(path, FileUser), user)
}

func stripTemplateMarker(body string) string {
	body = strings.ReplaceAll(body, TemplateMarker+"\n", "")
	body = strings.ReplaceAll(body, TemplateMarker, "")
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	return body + "\n"
}

func writeReplace(path, body string) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	name := temp.Name()
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("chmod %s: %w", filepath.Base(path), err)
	}
	if _, err := temp.WriteString(body); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return syncDir(dir)
}

// syncDir flushes a directory entry after a rename so the replacement
// survives a crash (rename alone is not guaranteed durable on all filesystems).
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
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
	return LoadWithLocale(path, mode, LocaleZH)
}

func LoadWithLocale(path string, mode Mode, locale Locale) (Snapshot, error) {
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
	// USER.md and MEMORY.md are optional for guests: guest snapshots drop
	// their content anyway, so a missing file must not take down every
	// guest turn. Owners keep the strict contract so a deleted identity
	// file surfaces as ErrMissing.
	user, err := readOptionalHomeFile(resolvedHome, FileUser, mode != ModeGuest)
	if err != nil {
		return Snapshot{}, err
	}
	memory, err := readOptionalHomeFile(resolvedHome, FileMemory, mode != ModeGuest)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Path: resolvedHome, Mode: mode, Soul: soul, User: user, Memory: memory}
	if mode != ModeOwner {
		snap.User = ""
		snap.Memory = ""
	}
	snap.Identity, snap.Prompt, snap.Warnings = compose(resolvedHome, mode, locale, soul, snap.User, snap.Memory)
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

// readOptionalHomeFile reads a home file, tolerating a missing file when
// strict is false (returning ""). All other errors (e.g. a symlink that
// escapes the home directory) are still returned.
func readOptionalHomeFile(resolvedHome, name string, strict bool) (string, error) {
	body, err := readHomeFile(resolvedHome, name)
	if err != nil && !strict && errors.Is(err, ErrMissing) {
		return "", nil
	}
	return body, err
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

func compose(path string, mode Mode, locale Locale, soul, user, memory string) (identity, prompt string, warnings []string) {
	wrapper := guestWrapper(locale)
	if mode == ModeOwner {
		wrapper = ownerWrapper(path, locale)
	}
	return composeWithWrapper(wrapper, mode, soul, user, memory)
}

func composeWithWrapper(wrapper string, mode Mode, soul, user, memory string) (identity, prompt string, warnings []string) {
	soul = truncateTo(soul, BudgetSoul)
	user = truncateTo(user, BudgetUser)
	memory = truncateTo(memory, BudgetMemory)

	parts := []string{wrapper}
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

// File is one of the three files of Steve's home as a page shows it:
// what it is for, what it holds, how much room it has.
type File struct {
	Name     string
	Text     string
	Budget   int
	Template bool
	Missing  bool
}

// Budgets is how much of each file reaches the prompt.
var Budgets = map[string]int{FileSoul: BudgetSoul, FileUser: BudgetUser, FileMemory: BudgetMemory}

// Files reads the three files as they are, for editing. A missing file
// is listed as missing rather than failing the read: the page is where
// it gets written.
func Files(path string) ([]File, error) {
	resolved, err := resolveHome(path)
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, 3)
	for _, name := range []string{FileSoul, FileUser, FileMemory} {
		f := File{Name: name, Budget: Budgets[name]}
		body, err := readHomeFile(resolved, name)
		if errors.Is(err, ErrMissing) {
			f.Missing = true
		} else if err != nil {
			return nil, err
		} else {
			f.Text, f.Template = body, IsTemplate(body)
		}
		out = append(out, f)
	}
	return out, nil
}

// Write replaces one of the three files. The text must fit the file's
// budget — what does not fit would be cut before the agent saw it — and
// a symlinked file is followed only inside the home. The write is a
// rename, so a reader sees the old file or the new one, never a torn one.
func Write(path, name, text string) error {
	budget, ok := Budgets[name]
	if !ok {
		return fmt.Errorf("%s is not one of %s, %s, %s", name, FileSoul, FileUser, FileMemory)
	}
	if len([]byte(text)) > budget {
		return fmt.Errorf("%s is %d bytes; only %d reach the agent — shorten it", name, len([]byte(text)), budget)
	}
	resolved, err := resolveHome(path)
	if err != nil {
		return err
	}
	target := filepath.Join(resolved, name)
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(target)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", name, err)
		}
		if err := withinHome(resolved, real); err != nil {
			return err
		}
		target = real
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+name+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name()) // the write is the finding; a stray temp dotfile is all a failed sweep costs
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name()) // the close is the finding; a stray temp dotfile is all a failed sweep costs
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = os.Remove(tmp.Name()) // the chmod is the finding; a stray temp dotfile is all a failed sweep costs
		return err
	}
	return os.Rename(tmp.Name(), target)
}
