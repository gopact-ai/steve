package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrFileChanged = errors.New("configuration file changed outside this process; restart before applying management changes")

func (c *Config) FileRevision() string { return c.sourceFingerprint }

func fingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *Config) rememberFileRevision(path string, raw []byte) {
	// Abs fails only when the working directory is gone. An empty source
	// path then fails every CheckFileRevision as "source path differs",
	// which refuses the write rather than risking the wrong file.
	c.sourcePath, _ = filepath.Abs(path)
	c.sourceFingerprint = fingerprint(raw)
}

// CheckFileRevision is an optimistic document check immediately before replace.
// The baseline comes from Load or an earlier committed Save, never a first write.
func (c *Config) CheckFileRevision(path string) error {
	if c.sourceFingerprint == "" {
		return nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if absolute != c.sourcePath {
		return fmt.Errorf("%w: source path differs", ErrFileChanged)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFileChanged, err)
	}
	if fingerprint(raw) != c.sourceFingerprint {
		return ErrFileChanged
	}
	return nil
}

// AdoptFileRevision carries a committed candidate's version to the live object
// when an application publishes only the configuration section it changed.
func (c *Config) AdoptFileRevision(saved *Config) {
	c.sourcePath, c.sourceFingerprint = saved.sourcePath, saved.sourceFingerprint
}
