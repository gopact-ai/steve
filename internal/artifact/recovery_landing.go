package artifact

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// RecoveryLink keeps an ordinary landing's nullable single Source unchanged.
// The episode owner supplies and validates the complete frozen source chain.
type RecoveryLink struct {
	Episode     string `json:"episode"`
	HeadVersion int64  `json:"head_version"`
}

type CanonicalCommit struct {
	Artifact string `json:"artifact"`
	Version  int64  `json:"version"`
	Parent   string `json:"parent"`
}

type recoveryLandingKey struct{}
type recoveryLandingPermit struct {
	link     RecoveryLink
	driver   ledger.Lease
	lifetime context.Context
}

// LandRecoveryOnce is the episode's stable landing consumer. The caller can
// supply neither its baseline, sources, target nor a replacement artifact.
func (s *Store) LandRecoveryOnce(ctx context.Context, id string, driver ledger.Lease) (Landing, error) {
	r, err := attempt.New(s.ledger).WorkspaceRecovery(ctx, id)
	if err != nil {
		return Landing{}, err
	}
	p, found, err := s.projects.Get(ctx, r.Project)
	if err != nil {
		return Landing{}, err
	}
	if !found || p.Home != r.Target || project.RecoveryIdentity(p) != r.Declaration {
		return Landing{}, attempt.ErrWorkspaceRecovery
	}
	link := RecoveryLink{Episode: r.ID, HeadVersion: r.FrozenHeadVersion}
	ctx = context.WithValue(ctx, recoveryLandingKey{}, &recoveryLandingPermit{link: link, driver: driver, lifetime: ctx})
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		_, err := checkRecoveryLandingPermitTx(ctx, tx, p, nil, nil, false)
		return err
	}); err != nil {
		return Landing{}, err
	}
	return s.LandOnce(ctx, attempt.RecoveryLandingID(r), p, r.Head.Artifact, "workspace recovery")
}

func recoveryPermit(ctx context.Context) *recoveryLandingPermit {
	permit, _ := ctx.Value(recoveryLandingKey{}).(*recoveryLandingPermit)
	return permit
}

func checkRecoveryLandingPermitTx(ctx context.Context, tx *ledger.Tx, p project.Project, land *Landing, held *ledger.Lease, newAdmission bool) (attempt.WorkspaceRecovery, error) {
	permit := recoveryPermit(ctx)
	if permit == nil {
		return attempt.WorkspaceRecovery{}, attempt.ErrWorkspaceRecovery
	}
	r, err := attempt.CheckRecoveryLandingTx(tx, permit.link.Episode, permit.link.HeadVersion, permit.driver, held, newAdmission)
	if err != nil {
		return r, err
	}
	if r.Project != p.ID || r.Target != p.Home || project.RecoveryIdentity(p) != r.Declaration {
		return r, attempt.ErrWorkspaceRecovery
	}
	if land != nil && (land.Recovery == nil || *land.Recovery != permit.link || land.ID != attempt.RecoveryLandingID(r) || land.Artifact != r.Head.Artifact || land.Base != r.Baseline.Artifact || land.Target != r.Target) {
		return r, errors.New("recovery landing differs from its frozen episode")
	}
	return r, nil
}

func (s *Store) admitRecoveryLandingSources(ctx context.Context, p project.Project, artifactID string) (context.Context, func(), error) {
	var r attempt.WorkspaceRecovery
	err := s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		var err error
		r, err = checkRecoveryLandingPermitTx(ctx, tx, p, nil, nil, true)
		return err
	})
	if err != nil {
		return ctx, func() {}, err
	}
	var scopes []*execution.Scope
	finish := func() {
		for i := len(scopes) - 1; i >= 0; i-- {
			scopes[i].Finish(nil)
		}
	}
	if s.executions != nil {
		for _, source := range r.Head.Sources {
			scope, err := s.executions.BeginAccepted(ctx, execution.Key{TaskID: source.Execution.TaskID, InstanceID: "recovery-land/" + artifactID, AttemptID: source.Attempt}, &source.Execution)
			if err != nil {
				finish()
				return ctx, func() {}, err
			}
			scopes = append(scopes, scope)
			ctx = scope.Context()
		}
	}
	return ctx, finish, nil
}

func (s *Store) recoveryCanonicalTx(ctx context.Context, tx *ledger.Tx, p project.Project, land *Landing, onto, target string) error {
	if _, err := checkRecoveryLandingPermitTx(ctx, tx, p, land, land.Lease, false); err != nil {
		return err
	}
	current, found, err := tx.Name(CanonicalRef(p.ID))
	if err != nil {
		return err
	}
	if !found || current.Artifact != onto {
		return ledger.ErrConflict
	}
	version := current.Version
	if onto != target {
		version, err = tx.CompareAndSetName(current.Name, current.Version, target)
		if err != nil {
			return err
		}
	}
	land.Committed = &CanonicalCommit{Artifact: target, Version: version, Parent: onto}
	return nil
}

func (s *Store) ensureRecoveryLandingReceipt(ctx context.Context, p project.Project, land Landing) error {
	if land.Committed == nil || land.Committed.Artifact == "" || land.Committed.Version < 1 {
		return errors.New("recovery landing lacks its canonical commit receipt")
	}
	if _, found, err := s.Manifest(ctx, land.Committed.Artifact); err != nil {
		return err
	} else if found {
		return nil
	}
	_, err := s.receipt(ctx, p, Manifest{ID: land.Committed.Artifact, Project: p.ID, Parent: land.Committed.Parent, Label: p.Level, By: land.ID, Message: "landed recovery " + land.Artifact, Canonical: true}, recordGuard{check: func(tx *ledger.Tx) error {
		_, err := checkRecoveryLandingPermitTx(ctx, tx, p, &land, nil, false)
		return err
	}})
	return err
}
