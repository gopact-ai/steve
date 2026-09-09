package plugins

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Prepare replays a durable receipt without rereading a source that might no
// longer exist. The expected digest binds every retry to the reviewed bytes.
func (s *Store) Prepare(ctx context.Context, commandID, digest string, source Source) (Receipt, error) {
	source, err := normalizeSource(source)
	if err != nil {
		return Receipt{}, err
	}
	if !nameShape.MatchString(commandID) || !digestShape.MatchString(digest) {
		return Receipt{}, fmt.Errorf("%w: command ID and expected digest are required", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := s.replay(ctx, commandID, digest, source); found || err != nil {
		return receipt, err
	}
	if pending, found, err := s.readPreparation(commandID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Receipt{}, err
	} else if found {
		if pending.Digest != digest || pending.Source != source {
			return Receipt{}, fmt.Errorf("%w: preparation command already used with different input", ErrConflict)
		}
		if bundle, err := s.Read(digest); err == nil {
			return s.Install(ctx, InstallRequest{CommandID: commandID, ExpectedDigest: digest, Source: source, Bundle: bundle})
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Receipt{}, err
		}
	}
	bundle, resolved, err := Resolve(ctx, source)
	if err != nil {
		return Receipt{}, err
	}
	return s.Install(ctx, InstallRequest{CommandID: commandID, ExpectedDigest: digest, Source: resolved, Bundle: bundle})
}

func normalizeSource(source Source) (Source, error) {
	if source.Kind == "directory" {
		if source.Location == "" {
			return source, fmt.Errorf("%w: source directory required", ErrInvalid)
		}
		location, err := filepath.Abs(source.Location)
		if err != nil {
			return source, err
		}
		source.Location = location
	}
	return source, source.validate()
}

func (s *Store) replay(ctx context.Context, id, digest string, source Source) (Receipt, bool, error) {
	if s.Dir == "" {
		return Receipt{}, false, fmt.Errorf("%w: store directory required", ErrInvalid)
	}
	_, found, err := s.readReceipt(id)
	if errors.Is(err, fs.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil || !found {
		return Receipt{}, false, err
	}
	if err := s.ensure(); err != nil {
		return Receipt{}, false, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return Receipt{}, false, err
	}
	defer unlock()
	receipt, found, err := s.readReceipt(id)
	if err != nil || !found {
		return receipt, found, err
	}
	if receipt.Digest != digest || receipt.Source != source {
		return Receipt{}, false, fmt.Errorf("%w: command already used with different input", ErrConflict)
	}
	bundle, err := s.Read(digest)
	if err != nil {
		return receipt, true, err
	}
	if bundle.Manifest.ID != receipt.ID || bundle.Manifest.Version != receipt.Version {
		return receipt, true, ErrIntegrity
	}
	return receipt, true, s.sync(filepath.Join(s.Dir, "receipts"))
}
