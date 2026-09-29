package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/i18n"
)

// InputError is a refusal of what the owner asked for, as opposed to a
// failure of this machine. Callers answer it as a bad request. A refusal
// this package makes is English until Say puts it in the owner's language;
// Message is one already worded, said as it is.
type InputError struct {
	Message string
	key     i18n.Key
	args    []any
}

func (e *InputError) Error() string { return e.Say(i18n.New(i18n.LocaleEN)) }

// Say is the refusal in text's language.
func (e *InputError) Say(text i18n.Catalog) string {
	if e.key == "" {
		return e.Message
	}
	return text.T(e.key, e.args...)
}

// failure is this machine failing to prepare a workspace: English until
// Say puts it in the owner's language, and still its cause underneath.
type failure struct {
	key   i18n.Key
	cause error
}

func (e failure) Error() string { return e.Say(i18n.New(i18n.LocaleEN)) }

// Say is the failure in text's language.
func (e failure) Say(text i18n.Catalog) string { return text.Errorf(e.key, e.cause).Error() }
func (e failure) Unwrap() error                { return e.cause }

// IsInputError reports whether err is a refusal the owner can act on.
func IsInputError(err error) bool {
	var input *InputError
	return errors.As(err, &input)
}

func refuse(key i18n.Key, args ...any) error {
	return &InputError{key: key, args: args}
}

// CheckWorkspaceProject confirms there is a default project and that it
// lives on this computer; the workspace page only ever moves a local one.
// local tells whether the project's home node is this machine.
func CheckWorkspaceProject(id string, local bool, node string) error {
	if id == "" {
		return refuse(i18n.DesktopWorkspaceNoProject)
	}
	if !local {
		return refuse(i18n.DesktopWorkspaceRemoteProject, id, node)
	}
	return nil
}

// ManagedWorkspace reports whether path is inside the desktop's own state
// directory, which is where a fresh installation keeps its first project
// until the owner chooses somewhere.
func ManagedWorkspace(stateDir, path string) bool {
	if stateDir == "" || path == "" {
		return false
	}
	state, path := filepath.Clean(stateDir), filepath.Clean(path)
	return path == state || within(path, state)
}

// PrepareWorkspace turns the owner's answer into an absolute directory that
// exists and can be written, creating it when needed. Places that belong to
// the operating system, the home directory itself, and the desktop's own
// state directory are refused, because agents will create and delete files
// there. stateDir is where the desktop keeps its configuration.
func PrepareWorkspace(input, stateDir string) (string, error) {
	path := strings.TrimSpace(input)
	if path == "" {
		return "", refuse(i18n.DesktopWorkspaceEmpty)
	}
	if strings.ContainsRune(path, 0) {
		return "", refuse(i18n.DesktopWorkspaceInvalid)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		return "", refuse(i18n.DesktopWorkspaceRelative)
	}
	path = filepath.Clean(path)
	if err := checkWorkspacePlace(resolveExisting(path), resolveExisting(filepath.Clean(home)), stateDir); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", failure{i18n.DesktopWorkspaceCreateFailed, err}
		}
	case err != nil:
		return "", failure{i18n.DesktopWorkspaceCheckFailed, err}
	case !info.IsDir():
		return "", refuse(i18n.DesktopWorkspaceNotFolder, path)
	}
	probe, err := os.CreateTemp(path, ".steve-write-*")
	if err != nil {
		return "", refuse(i18n.DesktopWorkspaceNotWritable, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return path, nil
}

// checkWorkspacePlace judges the resolved path: not the home or a volume
// root, not inside a system root, and not the desktop's state directory or
// any directory that contains it.
func checkWorkspacePlace(path, home, stateDir string) error {
	if path == home || path == filepath.VolumeName(path)+string(filepath.Separator) {
		return refuse(i18n.DesktopWorkspaceHomeOrRoot)
	}
	if stateDir != "" {
		state := resolveExisting(filepath.Clean(stateDir))
		if path == state || within(path, state) || within(state, path) {
			return refuse(i18n.DesktopWorkspaceStateDir)
		}
	}
	if within(path, home) {
		return nil
	}
	for _, reserved := range reservedRoots() {
		if path == reserved || within(path, reserved) {
			return refuse(i18n.DesktopWorkspaceSystem, reserved)
		}
	}
	return nil
}

func within(path, parent string) bool {
	return strings.HasPrefix(path, parent+string(filepath.Separator))
}

// resolveExisting follows symbolic links through the longest existing
// prefix of path, so a link into a refused place is judged by its target.
func resolveExisting(path string) string {
	rest := ""
	for current := path; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

func reservedRoots() []string {
	roots := []string{"/bin", "/sbin", "/usr", "/etc", "/var", "/dev", "/proc", "/sys", "/boot", "/lib", "/lib64", "/opt", "/System", "/Library", "/Applications", "/private", "/cores"}
	if system := os.Getenv("SystemRoot"); system != "" {
		roots = append(roots, filepath.Clean(system), filepath.Join(filepath.VolumeName(system), `\Program Files`), filepath.Join(filepath.VolumeName(system), `\Program Files (x86)`))
	}
	return roots
}
