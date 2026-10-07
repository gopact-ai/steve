package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func (s *Store) publishRuntimeSocket(root *os.Root, id, dir string) (string, error) {
	socket := filepath.Join(dir, "mcp.sock")
	if runtimeSocketPathLimit(runtime.GOOS) > 0 && !validRuntimeSocketPath(socket, runtime.GOOS) {
		return "", errors.Join(ErrIntegrity, os.Remove(dir))
	}
	raw, err := json.Marshal(socket)
	if err != nil {
		return "", errors.Join(err, os.Remove(dir))
	}
	if err := s.writeRecord(filepath.Join(s.RuntimeDir(id), "socket.json"), raw); err != nil {
		return "", runtimeSocketWriteError(root, "runtimes/"+id+"/socket.json", dir, socket, err)
	}
	return socket, nil
}

func runtimeSocketWriteError(root *os.Root, name, dir, socket string, cause error) error {
	// A write error can follow rename. Only a proven absent record permits
	// removing the fresh, empty directory; consumers may hold a published path.
	raw, err := readRegular(root, name, 4096)
	if os.IsNotExist(err) {
		removeErr := os.Remove(dir)
		if os.IsNotExist(removeErr) {
			removeErr = nil
		}
		if removeErr == nil {
			return cause
		}
		return errors.Join(cause, removeErr)
	}
	if err == nil {
		var location string
		if err = json.Unmarshal(raw, &location); err == nil && location == socket {
			return cause
		}
	}
	return errors.Join(cause, err, fmt.Errorf("%w: runtime socket publication is uncertain; allocated directory retained", ErrIntegrity))
}
