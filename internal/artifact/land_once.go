package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// LandOnce gives a durable caller one physical landing identity. Committed
// IDs return their result; interrupted application resumes its existing WAL.
func (s *Store) LandOnce(ctx context.Context, id string, p project.Project, artifactID, by string, source ...Source) (Landing, error) {
	if strings.TrimSpace(id) == "" {
		return Landing{}, errors.New("landing identity is required")
	}
	ttl := s.landingDriverTTL
	if ttl <= 0 {
		ttl = landTTL
	}
	drive, err := s.ledger.Acquire(ctx, "landing-driver:"+id, attempt.NewID(), ttl)
	if err != nil {
		return Landing{}, err
	}
	ctx, stop := s.startLandingDriver(ctx, drive, ttl)
	defer stop()
	op, found, err := s.ledger.Operation(ctx, id)
	if err != nil {
		return Landing{}, err
	}
	land := Landing{ID: id}
	if found {
		if op.Kind != landKind {
			return land, errors.New("landing identity belongs to another operation")
		}
		if err := json.Unmarshal(op.Data, &land); err != nil {
			return land, err
		}
		land.State = op.State
		if !land.Recoverable || land.Target != p.Home || land.Project != p.ID || land.Artifact != artifactID || !reflect.DeepEqual(land.Source, firstSource(source)) {
			return land, errors.New("landing identity has different source or result")
		}
		switch land.State {
		case LandCommitted:
			return land, s.ensureLandingReceipt(ctx, p, land)
		case LandMergeConflicted, LandApplyConflicted, LandCommitConflict:
			return land, Conflict{State: land.State, Paths: land.Paths}
		case LandApplying, LandRecoveryPending:
			cleanup, cancel := landingApplyContext(ctx)
			defer cancel()
			recovered, err := s.recoverLanding(cleanup, land)
			if err != nil {
				return recovered, err
			}
			if recovered.State != LandCommitted {
				return recovered, Conflict{State: recovered.State, Paths: recovered.Paths}
			}
			return recovered, nil
		case LandProposed:
		case LandLocked, LandMerged:
			from := land.State
			land.State, land.Lease, land.Now, land.Merged, land.Paths = LandProposed, nil, "", "", nil
			if _, err := s.ledger.Transition(ctx, id, from, LandProposed, by, landingFence(ctx, nil), nil, func(tx *ledger.Tx, op *ledger.Operation) error { return tx.SetData(op, land) }); err != nil {
				return land, err
			}
		default:
			return land, fmt.Errorf("landing %s has unknown state %s", id, land.State)
		}
	}
	return s.land(ctx, p, artifactID, by, nil, firstSource(source), &land)
}

func (s *Store) ensureLandingReceipt(ctx context.Context, p project.Project, land Landing) error {
	if land.Merged == "" || land.Merged == land.Now {
		return nil
	}
	if _, found, err := s.Manifest(ctx, land.Merged); err != nil || found {
		return err
	}
	_, err := s.receipt(ctx, p, Manifest{ID: land.Merged, Project: p.ID, Parent: land.Now, Label: p.Level, By: land.ID, Message: "landed " + short(land.Artifact), Canonical: true})
	return err
}
