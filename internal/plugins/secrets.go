package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

const MaxSecretBytes = 16 << 10

type localSecret struct {
	Reference SecretRef `json:"reference"`
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
}

type SecretInfo struct {
	Reference SecretRef `json:"reference"`
	CreatedAt time.Time `json:"created_at"`
}

func secretRevision(revision string) bool {
	if len(revision) != 32 {
		return false
	}
	raw, err := hex.DecodeString(revision)
	return err == nil && len(raw) == 16
}

// PutSecret creates a version rather than overwriting one an existing session
// may still use. The receipt intentionally reveals no value-derived digest.
func (s *Store) PutSecret(ctx context.Context, name, value string) (SecretInfo, error) {
	if !nameShape.MatchString(name) || value == "" || len(value) > MaxSecretBytes || !utf8.ValidString(value) {
		return SecretInfo{}, fmt.Errorf("%w: secret name and a bounded nonempty value are required", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return SecretInfo{}, err
	}
	if err := s.ensure(); err != nil {
		return SecretInfo{}, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return SecretInfo{}, err
	}
	defer unlock()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return SecretInfo{}, err
	}
	ref := SecretRef{Name: name, Revision: hex.EncodeToString(nonce[:])}
	record := localSecret{Reference: ref, Value: value, CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(record)
	if err != nil {
		return SecretInfo{}, err
	}
	if err := s.writeRecord(filepath.Join(s.Dir, "secrets", name+"-"+ref.Revision+".json"), raw); err != nil {
		return SecretInfo{}, err
	}
	return SecretInfo{Reference: ref, CreatedAt: record.CreatedAt}, nil
}

func (s *Store) Secret(ref SecretRef) (string, error) {
	if !nameShape.MatchString(ref.Name) || !secretRevision(ref.Revision) {
		return "", ErrUnavailable
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", ErrUnavailable
	}
	defer root.Close()
	raw, err := readRegular(root, "secrets/"+ref.Name+"-"+ref.Revision+".json", MaxManifestBytes)
	if err != nil {
		return "", ErrUnavailable
	}
	var secret localSecret
	if err := decodeStrict(raw, &secret); err != nil || secret.Reference != ref || secret.Value == "" || len(secret.Value) > MaxSecretBytes {
		return "", ErrUnavailable
	}
	return secret.Value, nil
}

func (s *Store) Secrets() ([]SecretInfo, error) {
	root, err := os.OpenRoot(s.Dir)
	if os.IsNotExist(err) {
		return []SecretInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), "secrets")
	if os.IsNotExist(err) {
		return []SecretInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]SecretInfo, 0, len(entries))
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			continue
		}
		raw, err := readRegular(root, "secrets/"+entry.Name(), MaxManifestBytes)
		if err != nil {
			return nil, err
		}
		var secret localSecret
		if err := decodeStrict(raw, &secret); err != nil {
			return nil, err
		}
		if entry.Name() != secret.Reference.Name+"-"+secret.Reference.Revision+".json" || !nameShape.MatchString(secret.Reference.Name) || !secretRevision(secret.Reference.Revision) {
			return nil, ErrIntegrity
		}
		out = append(out, SecretInfo{Reference: secret.Reference, CreatedAt: secret.CreatedAt})
	}
	return out, nil
}
