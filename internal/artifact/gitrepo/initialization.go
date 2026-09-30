package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const initializedMarker = "steve-initialized"

// Shadow repositories have their own identity and never detach maintenance
// processes that could outlive the operation using the repository.
var repositorySettings = [][2]string{
	{"user.name", "steve"}, {"user.email", "steve@localhost"},
	{"gc.auto", "0"}, {"gc.autoDetach", "false"}, {"maintenance.auto", "false"},
}

func initialized(dir string) (bool, error) {
	info, err := os.Lstat(filepath.Join(dir, initializedMarker))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("repository initialization marker is not a regular file: %s", dir)
	}
	return true, nil
}

// Initialization and configuration take place only in a private sibling.
// Cancellation may leave Git lock files there, but never in the published repo.
func createRepository(ctx context.Context, dir string) (_ *Repo, err error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+"-initializing-")
	if err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, os.RemoveAll(staging))
		}
	}()
	if _, err := git(ctx, "", nil, "init", "--bare", "--quiet", staging); err != nil {
		return nil, err
	}
	repo := &Repo{Dir: staging}
	for _, setting := range repositorySettings {
		if _, err := repo.Git(ctx, nil, "config", setting[0], setting[1]); err != nil {
			return nil, err
		}
	}
	if err := markInitialized(ctx, staging); err != nil {
		return nil, err
	}
	result, moved, err := publishRepository(ctx, staging, dir)
	published = moved
	return result, err
}

func publishRepository(ctx context.Context, staging, dir string) (*Repo, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// Rmdir removes only an empty directory, never a file or a repository
	// another process has published. Windows also needs an absent destination.
	if err := syscall.Rmdir(dir); err != nil && !os.IsNotExist(err) {
		if ready, readErr := initialized(dir); readErr == nil && ready {
			repo, err := validateRepository(ctx, dir)
			return repo, false, err
		}
		return nil, false, &os.PathError{Op: "publish repository", Path: dir, Err: err}
	}
	if err := os.Rename(staging, dir); err != nil {
		if ready, readErr := initialized(dir); readErr == nil && ready {
			repo, err := validateRepository(ctx, dir)
			return repo, false, err
		}
		return nil, false, err
	}
	return &Repo{Dir: dir}, true, nil
}

// A complete repository missing only our marker can be recognized without
// rewriting its configuration. Unknown partial trees and locks are not ours
// to repair: returning an error preserves their contents and writer evidence.
func recognizeRepository(ctx context.Context, dir string) (*Repo, error) {
	repo, err := validateRepository(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := markInitialized(ctx, dir); err != nil {
		return nil, err
	}
	return repo, nil
}

func validateRepository(ctx context.Context, dir string) (*Repo, error) {
	if err := noConfigurationLock(dir); err != nil {
		return nil, err
	}
	for _, part := range []struct {
		name string
		dir  bool
	}{{"HEAD", false}, {"config", false}, {"objects", true}, {"refs", true}} {
		info, err := os.Lstat(filepath.Join(dir, part.name))
		if err != nil {
			return nil, fmt.Errorf("existing shadow repository is incomplete; left unchanged: %w", err)
		}
		if part.dir && !info.IsDir() || !part.dir && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("existing shadow repository has an invalid %s; left unchanged", part.name)
		}
	}
	repo := &Repo{Dir: dir}
	bare, err := repo.Git(ctx, nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return nil, fmt.Errorf("inspect existing shadow repository: %w", err)
	}
	if strings.TrimSpace(bare) != "true" {
		return nil, fmt.Errorf("existing shadow repository is not a complete bare repository: %s", dir)
	}
	for _, setting := range repositorySettings {
		got, err := repo.Git(ctx, nil, "config", "--local", "--get", setting[0])
		if err != nil {
			return nil, fmt.Errorf("inspect existing shadow repository configuration: %w", err)
		}
		if strings.TrimSpace(got) != setting[1] {
			return nil, fmt.Errorf("existing shadow repository has an unrecognized %s; its configuration was left unchanged", setting[0])
		}
	}
	return repo, nil
}

func noConfigurationLock(dir string) error {
	_, err := os.Lstat(filepath.Join(dir, "config.lock"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("existing shadow repository has a configuration lock; left unchanged: %s", dir)
}

func markInitialized(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := noConfigurationLock(dir); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, initializedMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		_, err = initialized(dir)
		return err
	}
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString("1\n")
	return errors.Join(writeErr, file.Close())
}
