package home

import (
	"context"
	"fmt"
)

// IdentityEditor lets an onboarding conversation read and atomically replace
// the identity supplied by the application.
type IdentityEditor interface {
	Loader
	NeedsInit() (bool, error)
	WriteIdentity(context.Context, string, string) error
}

// EditableReader adds the application's atomic identity writer to a shared
// reader, without giving the home package its storage implementation.
type EditableReader struct {
	Reader
	SaveIdentity func(context.Context, string, string) error
}

func (r EditableReader) WriteIdentity(ctx context.Context, soul, user string) error {
	if r.SaveIdentity == nil {
		return fmt.Errorf("shared identity is read-only")
	}
	return r.SaveIdentity(ctx, soul, user)
}

// Reader loads a consistent shared identity view supplied by the application.
// It does not materialize files or assume the identity lives on this machine.
type Reader struct {
	ReadFiles func() (map[string]string, error)
	Label     string
	Locale    Locale
}

func (r Reader) Load(mode Mode) (Snapshot, error) {
	if mode == ModeNone {
		return Snapshot{Mode: ModeNone}, nil
	}
	if r.ReadFiles == nil {
		return Snapshot{}, ErrMissing
	}
	files, err := r.ReadFiles()
	if err != nil {
		return Snapshot{}, err
	}
	if _, found := files[FileSoul]; !found {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrMissing, FileSoul)
	}
	if mode == ModeOwner {
		for _, name := range []string{FileUser, FileMemory} {
			if _, found := files[name]; !found {
				return Snapshot{}, fmt.Errorf("%w: %s", ErrMissing, name)
			}
		}
	}
	label := r.Label
	if label == "" {
		label = "shared profile"
	}
	snapshot := Snapshot{Path: label, Mode: mode, Soul: files[FileSoul]}
	wrapper := guestWrapper(r.Locale)
	if mode == ModeOwner {
		snapshot.User, snapshot.Memory = files[FileUser], files[FileMemory]
		wrapper = "# Steve 共享档案\n\n身份与记忆由 Steve 保存并随协调节点同步。在档案页修改身份，使用记忆工具维护长期事实。新会话会读取最新内容。"
		if r.Locale == LocaleEN {
			wrapper = "# Steve shared profile\n\nSteve stores this identity and memory across coordinator changes. Edit identity in the profile page and use memory tools for durable facts. New sessions read the latest content."
		}
	}
	snapshot.Identity, snapshot.Prompt, snapshot.Warnings = composeWithWrapper(wrapper, mode, snapshot.Soul, snapshot.User, snapshot.Memory)
	return snapshot, nil
}

func (r Reader) NeedsInit() (bool, error) {
	if r.ReadFiles == nil {
		return true, ErrMissing
	}
	files, err := r.ReadFiles()
	if err != nil {
		return false, err
	}
	for _, name := range []string{FileSoul, FileUser} {
		if text, exists := files[name]; !exists || IsTemplate(text) {
			return true, nil
		}
	}
	return false, nil
}

// DefaultFiles returns independent initial identity contents without writing
// local files. The caller chooses its authoritative storage.
func DefaultFiles(locale Locale) map[string]string {
	pack := templatesFor(locale)
	return map[string]string{FileSoul: pack.soul, FileUser: pack.user, FileMemory: pack.memory}
}
