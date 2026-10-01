package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Recognition uses Git's exclusive-path protocol without changing config.
// Cooperative writers cannot replace an existing config.lock. Identity checks
// reject a replacement already visible, but are not conditional filesystem
// operations: forcibly removing an active lock or moving its directory outside
// this protocol is not safe while recognition is running.
type configurationLock struct {
	file *os.File
	info os.FileInfo
}

func lockConfiguration(ctx context.Context, dir string) (*configurationLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "config.lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("lock existing shadow repository configuration: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &configurationLock{file: file, info: info}, nil
}

func (l *configurationLock) held() error {
	info, err := os.Lstat(l.file.Name())
	if err != nil {
		return err
	}
	if !os.SameFile(l.info, info) {
		return errors.New("repository configuration lock was replaced; left unchanged")
	}
	return nil
}

func (l *configurationLock) release() error {
	// Close before removal for platforms that do not unlink an open file.
	closeErr := l.file.Close()
	if err := l.held(); err != nil {
		return errors.Join(closeErr, err)
	}
	return errors.Join(closeErr, os.Remove(l.file.Name()))
}
