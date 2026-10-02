package gitrepo

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const recoveryPreparationMarker = ".steve-workspace"

// PrepareRecovery publishes an owned container with an immutable base marker.
// The work directory is inside that container, so a working but empty tree can
// never be replaced by a late preparation. Existing content is only verified.
func (r *Repo) PrepareRecovery(ctx context.Context, base, work, recovery string) error {
	identity, err := hex.DecodeString(strings.TrimPrefix(recovery, "workspace-recovery-"))
	if err != nil || len(identity) != 16 || recovery != "workspace-recovery-"+hex.EncodeToString(identity) || !ValidSHA(base) || filepath.Base(work) != "work" {
		return errors.New("invalid recovery workspace identity")
	}
	container := filepath.Dir(work)
	marker := []byte(recovery + "\n" + base + "\n")
	verify := func() error {
		info, err := os.Lstat(container)
		if err != nil || !info.IsDir() {
			return ErrPreparedWorkspaceChanged
		}
		info, err = os.Lstat(filepath.Join(container, recoveryPreparationMarker))
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(marker)) {
			return ErrPreparedWorkspaceChanged
		}
		raw, err := os.ReadFile(filepath.Join(container, recoveryPreparationMarker))
		if err != nil || string(raw) != string(marker) {
			return ErrPreparedWorkspaceChanged
		}
		return r.VerifyCheckout(ctx, base, work)
	}
	if _, err := os.Lstat(container); err == nil {
		return verify()
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(container), 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(container), ".recovery-prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := r.Checkout(ctx, base, filepath.Join(stage, "work")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, recoveryPreparationMarker), marker, 0600); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(container); err == nil {
		return verify()
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, container); err != nil {
		if _, exists := os.Lstat(container); exists == nil {
			return verify()
		}
		return fmt.Errorf("publish recovery workspace: %w", err)
	}
	return nil
}
