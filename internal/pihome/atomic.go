package pihome

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

// writeAtomic replaces path with data by rename, leaving it alone when it
// already holds exactly that. A link in the way is replaced, never written
// through into the operator's own file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == mode {
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpName)
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, mode)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
	}
	return werr
}

// linkAtomic points path at target by rename, leaving a link that already
// does alone.
func linkAtomic(path, target string) error {
	if current, err := os.Readlink(path); err == nil && current == target {
		return nil
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	tmp := filepath.Join(filepath.Dir(path), tmpName+hex.EncodeToString(nonce[:]))
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
