package runtime

import (
	"os"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/harness"
)

func DshHome(stateDir string) string { return filepath.Join(stateDir, "runtimes", harness.Dsh) }

// DSH's launcher creates its shipped ACP profile in the isolated home. Link
// model credentials without inheriting custom profiles, Cordis patches or UI
// settings that could activate an external chat integration.
func PrepareDsh(dest, source string) error {
	if err := os.MkdirAll(filepath.Join(dest, "skills"), 0700); err != nil {
		return err
	}
	if source != "" {
		return linkAuth(dest, source, ".credentials.yaml")
	}
	return nil
}
