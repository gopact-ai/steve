package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/fsx"
)

// setupName is the per-machine record of how far the first-run guide has
// come. It sits beside the profile so a reset of the state directory also
// restarts the guide.
const setupName = "setup.json"

// SetupSteps are the guide's pages in order. "finished" is the page shown
// once everything before it is done or skipped.
var SetupSteps = []string{"identity", "workspace", "agents", "machines", "preferences", "finished"}

// SetupProgress is where the guide should open next. Done means the owner
// has been through the guide once; the console no longer opens it on its own.
type SetupProgress struct {
	Step      string    `json:"step"`
	Done      bool      `json:"done,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// ReadSetup returns the saved progress, or the guide's first page when
// nothing usable has been saved yet.
func ReadSetup(root string) (SetupProgress, error) {
	raw, err := os.ReadFile(filepath.Join(root, setupName))
	if errors.Is(err, os.ErrNotExist) {
		return SetupProgress{Step: SetupSteps[0]}, nil
	}
	if err != nil {
		return SetupProgress{}, fmt.Errorf("read setup progress: %w", err)
	}
	// Anything that does not decode to a known page is a fresh start: the
	// file is this machine's own note, so there is nobody to report it to.
	var progress SetupProgress
	if json.Unmarshal(raw, &progress) != nil || !slices.Contains(SetupSteps, progress.Step) {
		return SetupProgress{Step: SetupSteps[0]}, nil
	}
	return progress, nil
}

// SaveSetup records the page to open next. Once the guide is done it stays
// done: reopening a single page later must not bring the whole guide back.
func SaveSetup(root string, progress SetupProgress) error {
	if !slices.Contains(SetupSteps, progress.Step) {
		return refuse("unknown setup step %q", progress.Step)
	}
	if !progress.Done {
		current, err := ReadSetup(root)
		if err != nil {
			return err
		}
		progress.Done = current.Done
	}
	progress.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(progress, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(root, setupName)
	if err := fsx.WriteFile(path, append(raw, '\n')); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}
