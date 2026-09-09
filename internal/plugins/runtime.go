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
	"slices"
	"time"
)

// RuntimeConfig is the frozen machine-local launch configuration. It is never
// put in RuntimeRef: native provider environment can contain credentials.
type RuntimeConfig struct {
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	Env        []string `json:"env,omitempty"`
	ProcessDir string   `json:"process_dir,omitempty"`
	Permission string   `json:"permission,omitempty"`
}

// RuntimeRecord remains on its execution machine. Every new session receives
// its own home; reattachment loads it without rereading mutable defaults.
type RuntimeRecord struct {
	Ref        RuntimeRef    `json:"ref"`
	CommandID  string        `json:"command_id"`
	Config     RuntimeConfig `json:"config"`
	SkillsHash string        `json:"skills_hash"`
	CreatedAt  time.Time     `json:"created_at"`
}

type RuntimeMaterializer func(ctx context.Context, dir string, record RuntimeRecord) (string, error)

func (s *Store) PrepareRuntime(ctx context.Context, id string, selection Selection, cfg RuntimeConfig, materialize RuntimeMaterializer) (RuntimeRecord, error) {
	if !nameShape.MatchString(id) || cfg.Command == "" || materialize == nil {
		return RuntimeRecord{}, ErrInvalid
	}
	selection.Deployments = slices.Clone(selection.Deployments)
	slices.Sort(selection.Deployments)
	cfg = cfg.Clone()
	hash, err := selection.Hash()
	if err != nil {
		return RuntimeRecord{}, err
	}
	if _, err := s.Selection(selection); err != nil {
		return RuntimeRecord{}, err
	}
	if err := s.ensure(); err != nil {
		return RuntimeRecord{}, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return RuntimeRecord{}, err
	}
	defer unlock()
	if ref, found, err := s.runtimeCommand(id); err != nil || found {
		if err != nil {
			return RuntimeRecord{}, err
		}
		original, err := ref.Selection.Hash()
		if err != nil || original != hash {
			return RuntimeRecord{}, ErrConflict
		}
		record, err := s.Runtime(ref)
		if os.IsNotExist(err) {
			original, _, readErr := s.readRuntimeCommand(id)
			if readErr != nil {
				return record, readErr
			}
			return s.materializeRuntime(ctx, original, materialize)
		}
		if err != nil {
			return record, err
		}
		return record, s.sync(filepath.Join(s.Dir, "runtime-commands"))
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return RuntimeRecord{}, err
	}
	ref := RuntimeRef{ID: hex.EncodeToString(nonce[:]), Selection: selection}
	// The command is durable before any directory is prepared. An interrupted
	// operation keeps its native identity rather than allocating another home.
	record := RuntimeRecord{Ref: ref, CommandID: id, Config: cfg, CreatedAt: time.Now().UTC()}
	if err := s.writeRuntimeCommand(record); err != nil {
		return record, err
	}
	return s.materializeRuntime(ctx, record, materialize)
}

func (s *Store) materializeRuntime(ctx context.Context, record RuntimeRecord, materialize RuntimeMaterializer) (RuntimeRecord, error) {
	dir := s.RuntimeDir(record.Ref.ID)
	if _, err := os.Lstat(dir); err == nil {
		return s.Runtime(record.Ref)
	} else if !os.IsNotExist(err) {
		return record, err
	}
	stage, err := os.MkdirTemp(filepath.Join(s.Dir, "runtimes"), ".prepare-")
	if err != nil {
		return record, err
	}
	defer removeStaging(stage)
	record.SkillsHash, err = materialize(ctx, stage, record)
	if err != nil {
		return record, err
	}
	if !digestShape.MatchString(record.SkillsHash) {
		return record, ErrIntegrity
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return record, err
	}
	if err := writeSynced(filepath.Join(stage, "runtime.json"), raw, 0600); err != nil {
		return record, err
	}
	if err := syncRuntimeFiles(stage); err != nil {
		return record, err
	}
	if err := s.syncTree(stage); err != nil {
		return record, err
	}
	if err := ctx.Err(); err != nil {
		return record, err
	}
	if err := os.Rename(stage, dir); err != nil {
		return record, err
	}
	return record, s.sync(filepath.Dir(dir))
}

func (s *Store) ResumeRuntimePreparation(ctx context.Context, id string, selection Selection, materialize RuntimeMaterializer) (RuntimeRecord, bool, error) {
	if !nameShape.MatchString(id) {
		return RuntimeRecord{}, false, ErrInvalid
	}
	record, found, err := s.readRuntimeCommand(id)
	if err != nil || !found {
		return record, found, err
	}
	want, err := selection.Hash()
	if err != nil {
		return record, true, err
	}
	actual, err := record.Ref.Selection.Hash()
	if err != nil || actual != want {
		return record, true, ErrConflict
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return record, true, err
	}
	defer unlock()
	if _, err := s.Selection(selection); err != nil {
		return record, true, err
	}
	result, err := s.materializeRuntime(ctx, record, materialize)
	return result, true, err
}

func (s *Store) RuntimeDir(id string) string { return filepath.Join(s.Dir, "runtimes", id) }

func (s *Store) Runtime(ref RuntimeRef) (RuntimeRecord, error) {
	if err := ref.Validate(); err != nil {
		return RuntimeRecord{}, err
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return RuntimeRecord{}, err
	}
	defer root.Close()
	raw, err := readRegular(root, "runtimes/"+ref.ID+"/runtime.json", MaxManifestBytes)
	if err != nil {
		return RuntimeRecord{}, err
	}
	var record RuntimeRecord
	if err := decodeStrict(raw, &record); err != nil {
		return record, err
	}
	want, err := ref.Selection.Hash()
	if err != nil {
		return record, err
	}
	actual, err := record.Ref.Selection.Hash()
	if err != nil || actual != want || record.Ref.ID != ref.ID || record.Config.Command == "" || !digestShape.MatchString(record.SkillsHash) || record.CreatedAt.IsZero() {
		return record, ErrIntegrity
	}
	if _, err := s.Selection(ref.Selection); err != nil {
		return record, err
	}
	return record, nil
}

func (s *Store) writeRuntimeCommand(record RuntimeRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > MaxManifestBytes {
		return fmt.Errorf("%w: runtime configuration too large", ErrInvalid)
	}
	return s.writeRecord(filepath.Join(s.Dir, "runtime-commands", record.CommandID+".json"), raw)
}

func (s *Store) readRuntimeCommand(id string) (RuntimeRecord, bool, error) {
	root, err := os.OpenRoot(s.Dir)
	if os.IsNotExist(err) {
		return RuntimeRecord{}, false, nil
	}
	if err != nil {
		return RuntimeRecord{}, false, err
	}
	defer root.Close()
	raw, err := readRegular(root, "runtime-commands/"+id+".json", MaxManifestBytes)
	if os.IsNotExist(err) {
		return RuntimeRecord{}, false, nil
	}
	if err != nil {
		return RuntimeRecord{}, false, err
	}
	var record RuntimeRecord
	if err := decodeStrict(raw, &record); err != nil {
		return record, false, err
	}
	if record.CommandID != id || record.Ref.Validate() != nil || record.Config.Command == "" {
		return record, false, ErrIntegrity
	}
	return record, true, nil
}

func (s *Store) runtimeCommand(id string) (RuntimeRef, bool, error) {
	record, found, err := s.readRuntimeCommand(id)
	return record.Ref, found, err
}

func (cfg RuntimeConfig) Clone() RuntimeConfig {
	cfg.Args = slices.Clone(cfg.Args)
	cfg.Env = slices.Clone(cfg.Env)
	return cfg
}

func syncRuntimeFiles(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
}
