package plugins

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Prepared = "prepared"

type Source struct {
	Kind     string `json:"kind"`
	Location string `json:"location"`
	Commit   string `json:"commit,omitempty"`
	Subdir   string `json:"subdir,omitempty"`
}

type Receipt struct {
	Schema     int       `json:"schema"`
	CommandID  string    `json:"command_id"`
	ID         string    `json:"id"`
	Version    string    `json:"version"`
	Digest     string    `json:"digest"`
	Source     Source    `json:"source"`
	PreparedAt time.Time `json:"prepared_at"`
	State      string    `json:"state"`
}

type InstallRequest struct {
	CommandID      string
	ExpectedDigest string
	Source         Source
	Bundle         Bundle
}

// Store owns only prepared content. It has no pointer to configuration,
// harnesses, agents or the live skills map, so install cannot activate a tool.
type Store struct {
	Dir string
	// syncDir is injectable at the durability boundaries in package tests.
	syncDir func(string) error
}

func (s *Store) Install(ctx context.Context, req InstallRequest) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if !nameShape.MatchString(req.CommandID) || !digestShape.MatchString(req.ExpectedDigest) {
		return Receipt{}, fmt.Errorf("%w: command ID and expected digest are required", ErrInvalid)
	}
	bundle, err := DecodeBundle(req.Bundle.Data)
	if err != nil {
		return Receipt{}, err
	}
	if bundle.Digest != req.ExpectedDigest {
		return Receipt{}, fmt.Errorf("%w: preview differs from package", ErrIntegrity)
	}
	if err := req.Source.validate(); err != nil {
		return Receipt{}, err
	}
	if err := s.ensure(); err != nil {
		return Receipt{}, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer unlock()
	if receipt, found, err := s.readReceipt(req.CommandID); err != nil || found {
		if err != nil {
			return Receipt{}, err
		}
		if receipt.Digest != bundle.Digest || receipt.Source != req.Source || receipt.ID != bundle.Manifest.ID || receipt.Version != bundle.Manifest.Version {
			return Receipt{}, fmt.Errorf("%w: command already used with different input", ErrConflict)
		}
		if _, err := s.Read(receipt.Digest); err != nil {
			return Receipt{}, err
		}
		if err := s.sync(filepath.Join(s.Dir, "receipts")); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	if err := s.reservePreparation(req, bundle); err != nil {
		return Receipt{}, err
	}
	if err := s.publishBundle(ctx, bundle); err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{Schema: Schema, CommandID: req.CommandID, ID: bundle.Manifest.ID, Version: bundle.Manifest.Version, Digest: bundle.Digest, Source: req.Source, PreparedAt: time.Now().UTC(), State: Prepared}
	if err := s.writeReceipt(receipt); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (s *Store) ensure() error {
	if s.Dir == "" {
		return fmt.Errorf("%w: plugin store directory is required", ErrInvalid)
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: plugin store must be a directory", ErrIntegrity)
	}
	for _, name := range []string{"packages", "receipts", "requests"} {
		dir := filepath.Join(s.Dir, name)
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: store subdirectory %s", ErrIntegrity, name)
		}
	}
	if err := s.sync(s.Dir); err != nil {
		return err
	}
	return s.sync(filepath.Dir(s.Dir))
}

func (s *Store) Read(digest string) (Bundle, error) {
	if !digestShape.MatchString(digest) {
		return Bundle{}, fmt.Errorf("%w: package digest", ErrInvalid)
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return Bundle{}, err
	}
	defer root.Close()
	raw, err := readRegular(root, "packages/"+digest+"/bundle.tar", MaxPackageBytes)
	if err != nil {
		return Bundle{}, err
	}
	if contentDigest(raw) != digest {
		return Bundle{}, ErrIntegrity
	}
	bundle, err := DecodeBundle(raw)
	if err != nil {
		return Bundle{}, err
	}
	content, err := root.OpenRoot("packages/" + digest + "/content")
	if err != nil {
		return Bundle{}, err
	}
	defer content.Close()
	materialized, err := readRoot(context.Background(), content, false)
	if err != nil {
		return Bundle{}, err
	}
	if materialized.Digest != digest {
		return Bundle{}, fmt.Errorf("%w: materialized content changed", ErrIntegrity)
	}
	return bundle, nil
}

func (s *Store) List() ([]Receipt, error) {
	root, err := os.OpenRoot(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Receipt{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), "receipts")
	if errors.Is(err, fs.ErrNotExist) {
		return []Receipt{}, nil
	}
	if err != nil {
		return nil, err
	}
	receipts := make([]Receipt, 0, len(entries))
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			continue
		}
		id, err := recordID(entry.Name())
		if err != nil {
			return nil, err
		}
		receipt, found, err := s.readReceipt(id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: receipt disappeared", ErrIntegrity)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func (s *Store) sync(dir string) error {
	if s.syncDir != nil {
		return s.syncDir(dir)
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (source Source) validate() error {
	if len(source.Location) > 4096 || strings.ContainsAny(source.Location, "\x00\r\n") {
		return fmt.Errorf("%w: invalid source location", ErrInvalid)
	}
	switch source.Kind {
	case "directory":
		if !filepath.IsAbs(source.Location) || source.Commit != "" || source.Subdir != "" {
			return fmt.Errorf("%w: directory source", ErrInvalid)
		}
	case "git":
		if err := validateGitSource(source); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: source kind", ErrInvalid)
	}
	return nil
}
