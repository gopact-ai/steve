// Package localtoken owns the bearer token that guards a Hub's loopback
// console when the configuration names none. Loopback keeps other machines
// out, not other users and processes on this one; the token does.
package localtoken

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/fsx"
)

const (
	// FileName sits in the state directory, next to the data it protects.
	FileName = "loopback-token"
	// MinLength is what every local client checks before presenting a token.
	MinLength = 40
	// maxLength bounds what Read loads; generated tokens are 43 bytes.
	maxLength = 1024
)

// Resolve returns the token in dir, creating it on first use. Concurrent first
// startups agree on one token; a restart keeps it, so open pages stay signed in.
func Resolve(dir string) (string, error) {
	if token, err := Read(dir); !errors.Is(err, os.ErrNotExist) {
		return token, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var secret [32]byte
	_, _ = rand.Read(secret[:]) // crypto/rand.Read never fails
	if err := fsx.CreateFile(filepath.Join(dir, FileName), []byte(base64.RawURLEncoding.EncodeToString(secret[:]))); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return Read(dir)
}

// Read returns the token in dir; os.ErrNotExist means none was created yet.
// The file must be the private regular file it was created as: a link or a
// swap between the check and the read could hand another file's contents to
// every client as a bearer token.
func Read(dir string) (string, error) {
	path := filepath.Join(dir, FileName)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !private(info) {
		return "", fmt.Errorf("%s must be a private regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, opened) || !private(opened) {
		return "", fmt.Errorf("%s changed while it was read", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxLength+1))
	if err != nil {
		return "", err
	}
	token := string(raw)
	if len(token) < MinLength || len(token) > maxLength || strings.ContainsAny(token, "\r\n\t ") {
		return "", fmt.Errorf("%s does not hold a valid token", path)
	}
	return token, nil
}

func private(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0
}
