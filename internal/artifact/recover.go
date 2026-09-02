package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// LandRecoveryPending is a landing cut off while applying: the WAL says
// which paths were started; the canonical workspace says which landed.
const LandRecoveryPending = "recovery-pending"

// RecoverLandings is run at boot. A landing that never reached applying
// wrote nothing and is closed as interrupted. One cut off mid-apply goes
// recovery-pending, takes the canonical lock again under a new epoch, and
// finishes path by path: a path already at the merged content is skipped,
// one still at the old content is rewritten, anything else is a conflict
// that stops the commit but not the other paths.
func (s *Store) RecoverLandings(ctx context.Context) ([]Landing, error) {
	ops, err := s.ledger.Operations(ctx, landKind, "")
	if err != nil {
		return nil, err
	}
	var out []Landing
	for _, op := range ops {
		var land Landing
		if err := json.Unmarshal(op.Data, &land); err != nil {
			continue
		}
		land.State = op.State
		switch op.State {
		case LandProposed, LandLocked, LandMerged:
			land.Lease = nil
			_ = s.fail(ctx, &land, op.State, LandMergeConflicted, "interrupted before apply; nothing was written", nil)
			out = append(out, land)
		case LandApplying, LandRecoveryPending:
			recovered, err := s.recoverLanding(ctx, land)
			if err != nil {
				return out, err
			}
			out = append(out, recovered)
		}
	}
	return out, nil
}

func (s *Store) recoverLanding(ctx context.Context, land Landing) (Landing, error) {
	p, ok, err := s.projects.Get(ctx, land.Project)
	if err != nil || !ok {
		return land, fmt.Errorf("landing %s: project %s is unknown", land.ID, land.Project)
	}
	if land.State == LandApplying {
		land.Lease = nil
		if err := s.move(ctx, &land, LandApplying, LandRecoveryPending, nil); err != nil {
			return land, err
		}
	}
	// Nothing may be written before the lock is held again.
	lease, err := s.ledger.Acquire(ctx, "canonical:"+p.ID, land.ID, landTTL)
	if err != nil {
		return land, fmt.Errorf("landing %s: %w", land.ID, err)
	}
	land.Lease = &lease
	defer func() { _ = s.ledger.Release(context.WithoutCancel(ctx), lease) }()

	land.Round++
	journal := s.ledger.Journal()
	var conflicted, rewritten []string
	for _, path := range land.Paths {
		state, err := s.pathState(ctx, p, land, path)
		if err != nil {
			return land, err
		}
		switch state {
		case "merged":
			continue
		case "old":
			if _, err := journal.Started(ledger.EffectID{Operation: land.ID, Kind: "land-path", InstanceKey: fmt.Sprintf("%d/%s", land.Round, path)}, "", nil); err != nil {
				return land, err
			}
			if err := s.writeFromTree(ctx, p, land.Merged, path); err != nil {
				return land, err
			}
			if _, err := journal.Confirmed(ledger.EffectID{Operation: land.ID, Kind: "land-path", InstanceKey: fmt.Sprintf("%d/%s", land.Round, path)}, nil); err != nil {
				return land, err
			}
			rewritten = append(rewritten, path)
		default:
			conflicted = append(conflicted, path)
		}
	}
	if len(conflicted) > 0 {
		_ = s.fail(ctx, &land, LandRecoveryPending, LandApplyConflicted, "recovery: paths changed underneath", conflicted)
		return land, nil
	}
	current, _, _ := s.ledger.Name(ctx, CanonicalRef(p.ID))
	land.State = LandCommitted
	land.EndedAt = s.now().UTC()
	_, err = s.ledger.Transition(ctx, land.ID, LandRecoveryPending, LandCommitted, "recovery", []ledger.Lease{lease},
		map[string]any{"paths": land.Paths, "rewritten": rewritten, "round": land.Round},
		func(tx *ledger.Tx, op *ledger.Operation) error {
			if current.Artifact != land.Merged {
				if _, err := tx.CompareAndSetName(CanonicalRef(p.ID), current.Version, land.Merged); err != nil {
					return err
				}
			}
			return tx.SetData(op, land)
		})
	if err != nil {
		_ = s.fail(ctx, &land, LandRecoveryPending, LandCommitConflict, err.Error(), land.Paths)
		return land, nil
	}
	if _, err := s.receipt(ctx, p, Manifest{ID: land.Merged, Project: p.ID, Parent: land.Now, Label: p.Level, By: land.ID, Message: "landed " + short(land.Artifact) + " (recovered)", Canonical: true}); err != nil {
		return land, err
	}
	return land, nil
}

// pathState compares the canonical file with the merged and the old tree:
// "merged", "old", or "other".
func (s *Store) pathState(ctx context.Context, p project.Project, land Landing, path string) (string, error) {
	bare, err := s.bareFor(ctx, p)
	if err != nil {
		return "", err
	}
	script := fmt.Sprintf(
		"cd %s && cur=$( [ -e %s ] && git --git-dir=%s hash-object -- %s || echo missing ); "+
			"new=$(git --git-dir=%s rev-parse -q --verify %s 2>/dev/null || echo missing); "+
			"old=$(git --git-dir=%s rev-parse -q --verify %s 2>/dev/null || echo missing); "+
			"if [ \"$cur\" = \"$new\" ]; then echo merged; elif [ \"$cur\" = \"$old\" ]; then echo old; else echo other; fi",
		quote(p.Home.Path), quote(path), quote(bare), quote(path),
		quote(bare), quote(land.Merged+":"+path),
		quote(bare), quote(land.Now+":"+path))
	out, err := s.run(ctx, p.Home.Node, script)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	return lastLine(out), nil
}

// writeFromTree brings one canonical path to its content in tree, deleting
// it when the tree has none.
func (s *Store) writeFromTree(ctx context.Context, p project.Project, tree, path string) error {
	bare, err := s.bareFor(ctx, p)
	if err != nil {
		return err
	}
	script := fmt.Sprintf(
		"cd %s && if git --git-dir=%s rev-parse -q --verify %s >/dev/null 2>&1; then mkdir -p %s && git --git-dir=%s show %s > %s; else rm -f %s; fi",
		quote(p.Home.Path), quote(bare), quote(tree+":"+path), quote(filepath.Dir(path)), quote(bare), quote(tree+":"+path), quote(path), quote(path))
	_, err = s.run(ctx, p.Home.Node, script)
	return err
}

// bareFor is the shadow repository holding the project's objects where
// its canonical workspace lives.
func (s *Store) bareFor(ctx context.Context, p project.Project) (string, error) {
	if p.Home.Node == "" {
		return filepath.Join(s.Dir, "objects", p.ID+".git"), nil
	}
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return "", err
	}
	return nodeBare(state, p.ID), nil
}

// run executes a shell script where the project lives: here, or on a node.
func (s *Store) run(ctx context.Context, node, script string) (string, error) {
	if node != "" {
		return s.nodes.Exec(ctx, node, "", script)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
