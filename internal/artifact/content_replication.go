package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

const artifactContentKind = "artifact-content"

// objectLimiter is a replicator that bounds the bundle it will take; one
// that does not gets the package default.
type objectLimiter interface {
	MaxObjectBytes() int64
}

type contentReplicationState struct {
	mu       sync.Mutex
	projects map[string]*sync.Mutex
}

func (s *Store) contentLock(projectID string) *sync.Mutex {
	s.contentState.mu.Lock()
	defer s.contentState.mu.Unlock()
	if s.contentState.projects == nil {
		s.contentState.projects = map[string]*sync.Mutex{}
	}
	if s.contentState.projects[projectID] == nil {
		s.contentState.projects[projectID] = &sync.Mutex{}
	}
	return s.contentState.projects[projectID]
}

func (s *Store) prepareContent(ctx context.Context, m Manifest) (contentreplica.Manifest, error) {
	scope, err := s.replication.CheckLocal(ctx, m.Project)
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	if !m.Label.OrDefault().Admits(project.Level(scope.Level)) {
		return contentreplica.Manifest{}, contentreplica.ErrPlacement
	}
	// The source repo is already complete when receipt/record is called. Open
	// directly: calling Store.Repo here would recursively restore this record.
	repo, err := Open(ctx, filepath.Join(s.Dir, "objects", m.Project+".git"))
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	if _, err := s.verifyContentCommit(ctx, repo, m.ID); err != nil {
		return contentreplica.Manifest{}, err
	}
	file, err := os.CreateTemp("", "steve-artifact-content-*.bundle")
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	maxBytes := contentreplica.DefaultMaxObjectBytes
	if limited, ok := s.replication.(objectLimiter); ok {
		maxBytes = limited.MaxObjectBytes()
	}
	if err := repo.pin(ctx, m.ID); err != nil {
		return contentreplica.Manifest{}, err
	}
	hash := sha256.New()
	output := &bundleWriter{into: io.MultiWriter(file, hash), limit: maxBytes}
	cmd := exec.CommandContext(ctx, "git", "bundle", "create", "-", RefFor(m.ID))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_DIR="+repo.Dir)
	cmd.Stdout = output
	var stderr bytes.Buffer
	cmd.Stderr = &bundleWriter{into: &stderr, limit: 64 << 10}
	if err := cmd.Run(); err != nil {
		if output.exceeded {
			return contentreplica.Manifest{}, contentreplica.ErrTooLarge
		}
		return contentreplica.Manifest{}, fmt.Errorf("bundle artifact content: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return contentreplica.Manifest{}, err
	}
	ref := contentreplica.BlobRef{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: output.written}
	return s.replication.Prepare(ctx, m.Project, contentreplica.GitBundle, m.ID, ref, file)
}

type bundleWriter struct {
	into           io.Writer
	limit, written int64
	exceeded       bool
}

func (w *bundleWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.written {
		w.exceeded = true
		return 0, contentreplica.ErrTooLarge
	}
	n, err := w.into.Write(p)
	w.written += int64(n)
	return n, err
}

// restoreContent rebuilds a missing project cache from the ledger's immutable
// project/commit indexes. The bundle carries the complete commit closure, so
// restoration does not depend on the original coordinator or a parent chain.
func (s *Store) restoreContent(ctx context.Context, projectID string, repo *Repo) error {
	index, err := s.ledger.Bindings(ctx, artifactContentKind)
	if err != nil {
		return err
	}
	var commits []string
	for key := range index {
		if commit, ok := strings.CutPrefix(key, projectID+"/"); ok {
			commits = append(commits, commit)
		}
	}
	if len(commits) == 0 {
		return nil
	}
	if _, err := s.replication.CheckLocal(ctx, projectID); err != nil {
		return err
	}
	sort.Strings(commits)
	lock := s.contentLock(projectID)
	lock.Lock()
	defer lock.Unlock()
	// Share hashes within a pass, but start a fresh pass after repair: moving
	// a corrupt pack out of Git's lookup can affect another branch verified
	// earlier. Every registered closure must still be complete at return.
	for pass := 0; pass <= len(commits); pass++ {
		verified := map[string]int64{}
		repaired := false
		for _, commit := range commits {
			if !shaPattern.MatchString(commit) {
				return contentreplica.ErrIntegrity
			}
			if _, err := s.verifyContentCommitWith(ctx, repo, commit, verified); err == nil {
				continue
			}
			var id string
			if err := json.Unmarshal(index[projectID+"/"+commit], &id); err != nil {
				return contentreplica.ErrIntegrity
			}
			manifest, ok, err := contentreplica.Lookup(ctx, s.ledger, id)
			if err != nil {
				return err
			}
			if !ok {
				return contentreplica.ErrIncomplete
			}
			if manifest.Object.Scope.ProjectID != projectID || manifest.Object.Kind != contentreplica.GitBundle || manifest.Object.Key != commit {
				return contentreplica.ErrIntegrity
			}
			if err := s.restoreBundle(ctx, repo, commit, manifest, verified); err != nil {
				return err
			}
			repaired = true
			break
		}
		if !repaired {
			return nil
		}
	}
	return fmt.Errorf("%w: Git cache changed during repair", contentreplica.ErrIntegrity)
}

func (s *Store) restoreBundle(ctx context.Context, repo *Repo, commit string, manifest contentreplica.Manifest, verified map[string]int64) error {
	file, err := os.CreateTemp("", "steve-artifact-restore-*.bundle")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	manifest, err = s.replication.Read(ctx, manifest, file)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := contentreplica.Verify(ctx, file, manifest.Object.Blob); err != nil {
		return err
	}
	// Verify and import only the expected ref; a malformed bundle cannot
	// overwrite unrelated project refs even when its byte digest was correct.
	heads, err := repo.git(ctx, nil, "bundle", "list-heads", file.Name())
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(heads), "\n")
	if len(lines) != 1 {
		return errors.New("artifact content bundle has unexpected refs")
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[0] != commit || fields[1] != RefFor(commit) {
		return contentreplica.ErrIntegrity
	}
	stagingDir, err := os.MkdirTemp("", "steve-artifact-verified-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stagingDir)
	staging, err := Open(ctx, filepath.Join(stagingDir, "objects.git"))
	if err != nil {
		return err
	}
	if _, err := staging.git(ctx, nil, "bundle", "verify", file.Name()); err != nil {
		return err
	}
	if _, err := staging.git(ctx, nil, "fetch", "--quiet", file.Name(), RefFor(commit)+":"+RefFor(commit)); err != nil {
		return err
	}
	ids, err := s.verifyContentCommit(ctx, staging, commit)
	if err != nil {
		return fmt.Errorf("verify staged content: %w", err)
	}
	if err := expandContentPacks(ctx, staging); err != nil {
		return fmt.Errorf("expand staged content: %w", err)
	}
	if _, err := s.verifyContentCommit(ctx, staging, commit); err != nil {
		return fmt.Errorf("verify expanded content: %w", err)
	}
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, manifest); return err }); err != nil {
		return err
	}
	if err := installVerifiedObjects(staging, repo, ids); err != nil {
		return fmt.Errorf("install verified content: %w", err)
	}
	if err := quarantineInvalidContentPacks(ctx, repo); err != nil {
		return fmt.Errorf("quarantine invalid content: %w", err)
	}
	if _, err := s.verifyContentCommitWith(ctx, repo, commit, verified); err != nil {
		return fmt.Errorf("verify repaired content: %w", err)
	}
	_, err = repo.git(ctx, nil, "-c", "core.fsync=all", "update-ref", RefFor(commit), commit)
	return err
}
