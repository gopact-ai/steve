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
func recognizeRepository(ctx context.Context, dir string) (_ *Repo, err error) {
	lock, err := lockConfiguration(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.release()) }()
	repo, err := inspectRepository(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := lock.held(); err != nil {
		return nil, err
	}
	if err := writeInitializationMarker(ctx, dir); err != nil {
		return nil, err
	}
	return repo, nil
}

func validateRepository(ctx context.Context, dir string) (*Repo, error) {
	if err := noConfigurationLock(dir); err != nil {
		return nil, err
	}
	return inspectRepository(ctx, dir)
}

func inspectRepository(ctx context.Context, dir string) (*Repo, error) {
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
	raw, err := repo.Git(ctx, nil, "config", "--local", "--null", "--list")
	if err != nil {
		return nil, fmt.Errorf("inspect existing shadow repository configuration: %w", err)
	}
	settings := map[string]string{}
	for _, entry := range strings.Split(raw, "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		settings[key] = value
	}
	for _, setting := range repositorySettings {
		if settings[strings.ToLower(setting[0])] != setting[1] {
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
	if err := noConfigurationLock(dir); err != nil {
		return err
	}
	return writeInitializationMarker(ctx, dir)
}
