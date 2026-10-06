// Package launchconfig validates harness launch declarations without resolving
// executables or changing their arguments and environment.
package launchconfig

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/procgroup"
)

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks the declaration, not an adapter's generated command. Callers
// with a prepared adapter must check its command is unchanged and pass an empty
// command here. Arguments are literal argv entries; empty entries are valid.
func Validate(adapterName, command string, args, env []string) error {
	switch {
	case adapterName != "" && command != "":
		return errors.New("sets both adapter and command; pick one")
	case adapterName == "" && strings.TrimSpace(command) == "":
		return errors.New("needs an adapter or a command")
	case adapterName != "":
		if _, known := adapter.Catalog[adapterName]; !known {
			return fmt.Errorf("adapter %q is not one of %s", adapterName, strings.Join(adapter.Names(), ", "))
		}
		if len(args) > 0 {
			return errors.New("an adapter takes no args")
		}
	}
	if strings.ContainsRune(command, '\x00') {
		return errors.New("command contains NUL")
	}
	for i, arg := range args {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("argument %d contains NUL", i)
		}
	}
	seen := make(map[string]bool, len(env))
	for i, entry := range env {
		if strings.ContainsRune(entry, '\x00') {
			return fmt.Errorf("environment entry %d contains NUL", i)
		}
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !envKey.MatchString(key) {
			return fmt.Errorf("environment entry %d must be KEY=value with key matching [A-Za-z_][A-Za-z0-9_]*", i)
		}
		if key == procgroup.MarkVariable {
			return fmt.Errorf("environment key %q is reserved for process identity", key)
		}
		if seen[key] {
			return fmt.Errorf("duplicate environment key %q", key)
		}
		seen[key] = true
	}
	return nil
}
