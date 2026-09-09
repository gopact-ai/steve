package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

// RuntimeMCPToken is a loopback capability, distinct from the upstream secret.
// Its stable identity lets a reattached session keep its original MCP route.
func (s *Store) RuntimeMCPToken(ctx context.Context, ref RuntimeRef, name string) (string, error) {
	if !nameShape.MatchString(name) {
		return "", ErrInvalid
	}
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
	path := "runtimes/" + ref.ID + "/mcp-tokens/" + name
	raw, err := readRegular(root, path, 32)
	if err == nil {
		if !secretRevision(string(raw)) {
			return "", ErrIntegrity
		}
		return string(raw), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	dir := filepath.Join(s.RuntimeDir(ref.ID), "mcp-tokens")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(nonce[:])
	if err := writeSynced(filepath.Join(dir, name), []byte(token), 0600); err != nil {
		return "", err
	}
	if err := s.sync(dir); err != nil {
		return "", err
	}
	return token, s.sync(filepath.Dir(dir))
}
