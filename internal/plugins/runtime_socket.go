package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
		if !validRuntimeSocketPath(location) {
			if len(location) >= runtimeSocketPathLimit() {
				return "", fmt.Errorf("%w: cached runtime socket address is too long (%d bytes; maximum %d); saved address unchanged; coordinate existing consumers before repairing the cached address", ErrIntegrity, len(location), runtimeSocketPathLimit()-1)
			}
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
	// TMPDIR can be as long as the state directory; keep only sockets here.
	dir, err := os.MkdirTemp("/tmp", "steve-plugin-socket-")
	if err != nil {
		return "", err
	}
	socket := filepath.Join(dir, "mcp.sock")
	if !validRuntimeSocketPath(socket) {
		_ = os.Remove(dir)
		return "", ErrIntegrity
	}
	raw, err = json.Marshal(socket)
	if err != nil {
		return "", err
	}
	if err := s.writeRecord(filepath.Join(s.RuntimeDir(ref.ID), "socket.json"), raw); err != nil {
		return "", err
	}
	return socket, nil
}

func validRuntimeSocketPath(location string) bool {
	return len(location) < runtimeSocketPathLimit() && filepath.IsAbs(location) &&
		filepath.Base(location) == "mcp.sock" &&
		strings.HasPrefix(filepath.Base(filepath.Dir(location)), "steve-plugin-socket-")
}

func runtimeSocketPathLimit() int {
	// Filesystem Unix addresses need a trailing NUL; Darwin has 104 bytes.
	if runtime.GOOS == "linux" {
		return 108
	}
	return 104
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
