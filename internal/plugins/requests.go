package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type preparation struct {
	Schema    int    `json:"schema"`
	CommandID string `json:"command_id"`
	ID        string `json:"id"`
	Version   string `json:"version"`
	Digest    string `json:"digest"`
	Source    Source `json:"source"`
}

// Reserving the release identity before publishing bytes makes a crash in
// between publication and receipt creation replayable without reusing a name
// for different content. A failed preparation retains its original identity.
func (s *Store) reservePreparation(req InstallRequest, bundle Bundle) error {
	wanted := preparation{Schema: Schema, CommandID: req.CommandID, ID: bundle.Manifest.ID, Version: bundle.Manifest.Version, Digest: bundle.Digest, Source: req.Source}
	path := filepath.Join(s.Dir, "requests", req.CommandID+".json")
	if existing, found, err := s.readPreparation(req.CommandID); found || err != nil {
		if err != nil {
			return err
		}
		if existing != wanted {
			return fmt.Errorf("%w: preparation command already used with different input", ErrConflict)
		}
		return s.sync(filepath.Dir(path))
	}
	if _, err := s.List(); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, "requests"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			continue
		}
		id, err := recordID(entry.Name())
		if err != nil {
			return err
		}
		existing, found, err := s.readPreparation(id)
		if err != nil {
			return err
		}
		if !found {
			return ErrIntegrity
		}
		if existing.ID == wanted.ID && existing.Version == wanted.Version && existing.Digest != wanted.Digest {
			return fmt.Errorf("%w: %s@%s already has different content", ErrConflict, wanted.ID, wanted.Version)
		}
	}
	raw, err := json.Marshal(wanted)
	if err != nil {
		return err
	}
	return s.writeRecord(path, raw)
}

func recordID(name string) (string, error) {
	if filepath.Ext(name) != ".json" {
		return "", fmt.Errorf("%w: unexpected record entry", ErrIntegrity)
	}
	id := name[:len(name)-5]
	if !nameShape.MatchString(id) {
		return "", ErrIntegrity
	}
	return id, nil
}

func (s *Store) readPreparation(id string) (preparation, bool, error) {
	if !nameShape.MatchString(id) {
		return preparation{}, false, ErrInvalid
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return preparation{}, false, err
	}
	defer root.Close()
	raw, err := readRegular(root, "requests/"+id+".json", MaxManifestBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return preparation{}, false, nil
	}
	if err != nil {
		return preparation{}, false, err
	}
	var p preparation
	if err := decodeStrict(raw, &p); err != nil {
		return p, false, err
	}
	if p.Schema != Schema || p.CommandID != id || !ValidID(p.ID) || !validVersion(p.Version) || !digestShape.MatchString(p.Digest) {
		return p, false, ErrIntegrity
	}
	if err := p.Source.validate(); err != nil {
		return p, false, err
	}
	return p, true, nil
}
