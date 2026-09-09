package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// RuntimeSocket keeps a stable, short socket address even when the node's
// state directory is longer than the Unix socket pathname limit.
func (s *Store) RuntimeSocket(ctx context.Context, ref RuntimeRef) (string, error) {
	if _, err := s.Runtime(ref); err != nil {
		return "", err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	raw, err := readRegular(root, "runtimes/"+ref.ID+"/socket.json", 4096)
	if err == nil {
		var location string
		if err := json.Unmarshal(raw, &location); err != nil {
			return "", ErrIntegrity
		}
		if !filepath.IsAbs(location) || filepath.Base(location) != "mcp.sock" || !strings.HasPrefix(filepath.Base(filepath.Dir(location)), "steve-plugin-socket-") {
			return "", ErrIntegrity
		}
		if err := ensureSocketDirectory(filepath.Dir(location)); err != nil {
			return "", err
		}
		return location, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	dir, err := os.MkdirTemp("", "steve-plugin-socket-")
	if err != nil {
		return "", err
	}
	socket := filepath.Join(dir, "mcp.sock")
	raw, err = json.Marshal(socket)
	if err != nil {
		return "", err
	}
	if err := s.writeRecord(filepath.Join(s.RuntimeDir(ref.ID), "socket.json"), raw); err != nil {
		return "", err
	}
	return socket, nil
}

func ensureSocketDirectory(dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrIntegrity
	}
	return nil
}
