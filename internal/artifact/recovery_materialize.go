package artifact

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// PlanRecoveryWorkspace fixes the owned location before any checkout begins.
func (s *Store) PlanRecoveryWorkspace(ctx context.Context, r attempt.WorkspaceRecovery, node string) (project.Workspace, error) {
	p, found, err := s.projects.Get(ctx, r.Project)
	if err != nil {
		return project.Workspace{}, err
	}
	if !found || project.RecoveryIdentity(p) != r.Declaration {
		return project.Workspace{}, attempt.ErrWorkspaceRecovery
	}
	if err := s.admits(ctx, p, node); err != nil {
		return project.Workspace{}, err
	}
	root := s.Dir
	if node != "" {
		version, workspaceRoot, _, err := s.nodes.Git(ctx, node)
		if err != nil {
			return project.Workspace{}, err
		}
		if version == "" {
			return project.Workspace{}, errors.New("recovery destination has no Git")
		}
		root = workspaceRoot
	}
	if root == "" {
		return project.Workspace{}, errors.New("recovery destination has no workspace root")
	}
	id := "wt-" + short(r.Baseline.Artifact) + "-" + r.ID
	work := filepath.Join(root, "worktrees", id, "work")
	if node == r.Target.Node && (work == filepath.Clean(r.Target.Path) || strings.HasPrefix(work, strings.TrimSuffix(filepath.Clean(r.Target.Path), string(filepath.Separator))+string(filepath.Separator))) {
		return project.Workspace{}, errors.New("recovery destination is inside the original isolated directory")
	}
	return project.Workspace{RecoveryID: r.ID, ID: id, Project: r.Project, Node: node, Path: work, Kind: project.KindWorktree, Base: r.Baseline.Artifact}, nil
}

// PrepareRecoveryWorkspace is only used while the recorded preparation has not
// admitted a writer. A replay validates an existing prepared checkout, not a
// working directory; publication never removes unknown content.
func (s *Store) PrepareRecoveryWorkspace(ctx context.Context, r attempt.WorkspaceRecovery) error {
	ws, err := s.PlanRecoveryWorkspace(ctx, r, r.Workspace.Node)
	if err != nil {
		return err
	}
	if ws != r.Workspace {
		return errors.New("recovery materialization location changed")
	}
	p, _, err := s.projects.Get(ctx, r.Project)
	if err != nil {
		return err
	}
	if err := s.ledger.Read(ctx, func(tx *ledger.ReadTx) error {
		content, err := s.RecoveryOutputTx(tx, p.ID, r.Baseline.Artifact, r.Baseline.Artifact)
		if err != nil {
			return err
		}
		if content.ID != r.Baseline.ContentID || content.Storage != r.Baseline.Storage {
			return errors.New("recovery baseline content identity changed")
		}
		return nil
	}); err != nil {
		return err
	}
	hub, err := s.Repo(ctx, p.ID)
	if err != nil {
		return err
	}
	if ws.Node == "" {
		return hub.PrepareRecovery(ctx, ws.Base, ws.Path, r.ID)
	}
	_, _, state, err := s.nodes.Git(ctx, ws.Node)
	if err != nil {
		return err
	}
	bare := nodeBare(state, p.ID)
	if _, err = s.nodes.Artifact(ctx, ws.Node, ops.Request{Op: ops.Init, Repo: bare}); err != nil {
		return err
	}
	if err := s.ensureOnNode(ctx, p, ws.Node, bare, hub, ws.Base); err != nil {
		return err
	}
	_, err = s.nodes.Artifact(ctx, ws.Node, ops.Request{Op: ops.PrepareRecovery, Repo: bare, Commit: ws.Base, WorkTree: ws.Path, Recovery: r.ID})
	return err
}

// PublishRecovery preserves all ordinary project paths, including inputs/.
// A shared recovery checkout has no per-attempt injected input directory.
func (s *Store) PublishRecovery(ctx context.Context, ws project.Workspace, parent, by, message string) (Manifest, bool, error) {
	if ws.RecoveryID == "" || ws.Kind != project.KindWorktree || parent == "" {
		return Manifest{}, false, errors.New("recovery publication requires its fixed owner and parent")
	}
	var publisher attempt.Record
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		var err error
		publisher, err = attempt.RecoveryPublisherTx(tx, by, ws, parent, s.ledger.Region())
		return err
	}); err != nil {
		return Manifest{}, false, err
	}
	for _, lease := range publisher.Leases {
		if err := s.ledger.CheckAny(ctx, lease); err != nil {
			return Manifest{}, false, err
		}
	}
	p, found, err := s.projects.Get(ctx, ws.Project)
	if err != nil {
		return Manifest{}, false, err
	}
	if !found {
		return Manifest{}, false, project.ErrUnknown
	}
	if err := s.admits(ctx, p, ws.Node); err != nil {
		return Manifest{}, false, err
	}
	hub, err := s.Repo(ctx, p.ID)
	if err != nil {
		return Manifest{}, false, err
	}
	var sha string
	var changed bool
	if ws.Node == "" {
		sha, changed, err = hub.Snapshot(ctx, ws.Path, parent, message, false)
	} else {
		sha, changed, _, err = s.snapshotOnNode(ctx, ws.Node, p, ws.Path, parent, message, hub, false)
	}
	if err != nil {
		return Manifest{}, false, err
	}
	if !changed {
		m, found, err := s.Manifest(ctx, sha)
		if err != nil {
			return m, false, err
		}
		if !found {
			return m, false, errors.New("recovery parent manifest disappeared")
		}
		return m, false, nil
	}
	m, err := s.receipt(ctx, p, Manifest{ID: sha, Project: p.ID, Parent: parent, Label: p.Level, By: by, Message: message})
	return m, changed, err
}
