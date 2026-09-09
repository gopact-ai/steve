package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

var ErrRuntimeRetired = errors.New("plugin runtime is retired")
var ErrRuntimeBusy = errors.New("plugin runtime still has unconfirmed process use")

type RuntimeUse struct {
	ID        string     `json:"id"`
	Runtime   RuntimeRef `json:"runtime"`
	Kind      string     `json:"kind"`
	Stopped   bool       `json:"stopped"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type RuntimePackage struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type RuntimeInfo struct {
	Packages  []RuntimePackage `json:"packages"`
	Ref       RuntimeRef       `json:"ref"`
	CommandID string           `json:"command_id"`
	CreatedAt time.Time        `json:"created_at"`
	Retired   bool             `json:"retired"`
	Uses      []RuntimeUse     `json:"uses"`
}

func (s *Store) BeginRuntimeUse(ctx context.Context, ref RuntimeRef, id, kind string) error {
	if id == "" || len(id) > 512 || !nameShape.MatchString(kind) {
		return ErrInvalid
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.CheckRuntimeActive(ref); err != nil {
		return err
	}
	usage := RuntimeUse{ID: id, Runtime: *ref.Clone(), Kind: kind, UpdatedAt: time.Now().UTC()}
	raw, err := json.Marshal(usage)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.RuntimeDir(ref.ID), "uses")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := s.writeRecord(filepath.Join(dir, contentDigest([]byte(id))+".json"), raw); err != nil {
		return err
	}
	return s.sync(filepath.Dir(dir))
}

func (s *Store) EndRuntimeUse(ctx context.Context, ref RuntimeRef, id string) error {
	if ref.Validate() != nil || id == "" || len(id) > 512 {
		return ErrInvalid
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	path := "runtimes/" + ref.ID + "/uses/" + contentDigest([]byte(id)) + ".json"
	raw, err := readRegular(root, path, MaxManifestBytes)
	if err != nil {
		return err
	}
	var usage RuntimeUse
	if err := decodeStrict(raw, &usage); err != nil {
		return err
	}
	if usage.ID != id || usage.Runtime.ID != ref.ID {
		return ErrIntegrity
	}
	usage.Stopped = true
	usage.UpdatedAt = time.Now().UTC()
	raw, err = json.Marshal(usage)
	if err != nil {
		return err
	}
	return s.writeRecord(filepath.Join(s.Dir, filepath.FromSlash(path)), raw)
}

func (s *Store) CheckRuntimeActive(ref RuntimeRef) error {
	if ref.Validate() != nil {
		return ErrInvalid
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "removed-runtimes", ref.ID)); err == nil {
		return ErrRuntimeRetired
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(s.RuntimeDir(ref.ID), "retired.json")); err == nil {
		return ErrRuntimeRetired
	} else if !os.IsNotExist(err) {
		return err
	}
	_, err := s.Runtime(ref)
	return err
}

func (s *Store) RuntimeInfos() ([]RuntimeInfo, error) {
	root, err := os.OpenRoot(s.Dir)
	if os.IsNotExist(err) {
		return []RuntimeInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), "runtime-commands")
	if os.IsNotExist(err) {
		return []RuntimeInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	receipts, err := s.List()
	if err != nil {
		return nil, err
	}
	versions := map[string]Receipt{}
	for _, receipt := range receipts {
		versions[receipt.Digest] = receipt
	}
	infos := make([]RuntimeInfo, 0, len(entries))
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			continue
		}
		id, err := recordID(entry.Name())
		if err != nil {
			return nil, err
		}
		record, found, err := s.readRuntimeCommand(id)
		if err != nil || !found {
			return nil, errors.Join(ErrIntegrity, err)
		}
		removed := false
		if _, removedErr := root.Lstat("removed-runtimes/" + record.Ref.ID); removedErr == nil {
			removed = true
			if _, dirErr := root.Lstat("runtimes/" + record.Ref.ID); os.IsNotExist(dirErr) {
				continue
			} else if dirErr != nil {
				return nil, dirErr
			}
		} else if !os.IsNotExist(removedErr) {
			return nil, removedErr
		}
		info := RuntimeInfo{Ref: record.Ref, CommandID: id, CreatedAt: record.CreatedAt, Uses: []RuntimeUse{}}
		for _, hash := range record.Ref.Selection.Deployments {
			deployment, err := s.Deployment(hash)
			if err != nil {
				return nil, err
			}
			receipt, ok := versions[deployment.Deployment.Digest]
			if !ok {
				return nil, ErrIntegrity
			}
			info.Packages = append(info.Packages, RuntimePackage{ID: receipt.ID, Version: receipt.Version, Digest: receipt.Digest})
		}
		_, err = root.Lstat("runtimes/" + record.Ref.ID + "/retired.json")
		info.Retired = removed || err == nil
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		uses, err := fs.ReadDir(root.FS(), "runtimes/"+record.Ref.ID+"/uses")
		if os.IsNotExist(err) {
			infos = append(infos, info)
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, use := range uses {
			if len(use.Name()) > 0 && use.Name()[0] == '.' {
				continue
			}
			raw, err := readRegular(root, "runtimes/"+record.Ref.ID+"/uses/"+use.Name(), MaxManifestBytes)
			if err != nil {
				return nil, err
			}
			var usage RuntimeUse
			if err := decodeStrict(raw, &usage); err != nil {
				return nil, err
			}
			if usage.Runtime.ID != record.Ref.ID || use.Name() != contentDigest([]byte(usage.ID))+".json" {
				return nil, ErrIntegrity
			}
			info.Uses = append(info.Uses, usage)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// RetireRuntime closes admission before checking stop evidence. It never kills
// a process: a caller with outstanding use must finish that use explicitly.
func (s *Store) RetireRuntime(ctx context.Context, ref RuntimeRef) error {
	if ref.Validate() != nil {
		return ErrInvalid
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := s.RuntimeInfo(ref.ID)
	if err != nil {
		return err
	}
	want, _ := ref.Selection.Hash()
	actual, err := info.Ref.Selection.Hash()
	if err != nil || want != actual {
		return ErrIntegrity
	}
	if err := os.MkdirAll(s.RuntimeDir(ref.ID), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Ref RuntimeRef `json:"ref"`
		At  time.Time  `json:"at"`
	}{ref, time.Now().UTC()})
	if err != nil {
		return err
	}
	return s.writeRecord(filepath.Join(s.RuntimeDir(ref.ID), "retired.json"), raw)
}

func (s *Store) RemoveRuntime(ctx context.Context, ref RuntimeRef) error {
	if ref.Validate() != nil {
		return ErrInvalid
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	infos, err := s.RuntimeInfos()
	if err != nil {
		return err
	}
	var chosen *RuntimeInfo
	for i := range infos {
		if infos[i].Ref.ID == ref.ID {
			chosen = &infos[i]
			break
		}
	}
	if chosen == nil {
		return s.sync(filepath.Join(s.Dir, "runtimes"))
	}
	want, _ := ref.Selection.Hash()
	actual, err := chosen.Ref.Selection.Hash()
	if err != nil || want != actual {
		return ErrIntegrity
	}
	if !chosen.Retired {
		return fmt.Errorf("%w: runtime admission must be retired first", ErrRuntimeBusy)
	}
	for _, usage := range chosen.Uses {
		if !usage.Stopped {
			return ErrRuntimeBusy
		}
	}
	// Keep the command tombstone so an uncertain retry cannot recreate a
	// deleted runtime under the same original command identity.
	if err := s.writeRecord(filepath.Join(s.Dir, "removed-runtimes", ref.ID), []byte(`{"removed":true}`)); err != nil {
		return err
	}
	if err := os.RemoveAll(s.RuntimeDir(ref.ID)); err != nil {
		return err
	}
	return s.sync(filepath.Join(s.Dir, "runtimes"))
}

func (s *Store) RuntimeInfo(id string) (RuntimeInfo, error) {
	infos, err := s.RuntimeInfos()
	if err != nil {
		return RuntimeInfo{}, err
	}
	for _, info := range infos {
		if info.Ref.ID == id {
			return info, nil
		}
	}
	return RuntimeInfo{}, ErrUnavailable
}

func (s *Store) CheckRuntimeRemovable(ref RuntimeRef) error {
	if ref.Validate() != nil {
		return ErrInvalid
	}
	info, err := s.RuntimeInfo(ref.ID)
	if err != nil {
		return err
	}
	if !info.Retired {
		return ErrRuntimeBusy
	}
	for _, usage := range info.Uses {
		if !usage.Stopped {
			return ErrRuntimeBusy
		}
	}
	return nil
}
