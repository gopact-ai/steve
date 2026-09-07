// Package hubid owns durable process-instance identity, independent of hostname.
package hubid

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const FileName = "hub-identity.json"

var idShape = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

type Identity struct {
	ID string `json:"id"`
}

// Resolve assigns an identity once. Moving or restoring this state directory
// retains it; changing a configured identity requires an explicit migration.
func Resolve(stateDir, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" && !idShape.MatchString(configured) {
		return "", errors.New("invalid gateway.hub_id")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Join(stateDir, FileName)
	read := func() (string, error) {
		raw, err := os.ReadFile(name)
		if err != nil {
			return "", err
		}
		var stored Identity
		if err := json.Unmarshal(raw, &stored); err != nil {
			return "", fmt.Errorf("read hub identity: %w", err)
		}
		if !idShape.MatchString(stored.ID) {
			return "", errors.New("stored hub identity is invalid")
		}
		if configured != "" && configured != stored.ID {
			return "", errors.New("configured hub identity differs from persisted identity; explicit migration is required")
		}
		return stored.ID, nil
	}
	if id, err := read(); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id := configured
	if id == "" {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return "", err
		}
		id = "hub-" + hex.EncodeToString(bytes[:])
	}
	raw, _ := json.Marshal(Identity{ID: id})
	tmp, err := os.CreateTemp(stateDir, ".hub-identity-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	// Link publishes without overwriting another simultaneous first startup.
	if err := os.Link(tmp.Name(), name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return read()
		}
		return "", err
	}
	d, err := os.Open(stateDir)
	if err != nil {
		return "", err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return "", err
	}
	return id, nil
}
