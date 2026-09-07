package project

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

const cloneKind = "workspace-clone"

var ErrCloneIsolated = errors.New("workspace clone is running or unconfirmed; confirm it stopped before changing ownership")

// CloneOperation is management work, separate from agent attempts and usage.
// Its durable isolation outlives a lease timeout or a disconnected requester.
type CloneOperation struct {
	ID       string       `json:"id"`
	Project  string       `json:"project"`
	Copy     Copy         `json:"copy"`
	Lease    ledger.Lease `json:"lease"`
	State    string       `json:"state"`
	Source   string       `json:"source"`
	Evidence string       `json:"evidence,omitempty"`
	Error    string       `json:"error,omitempty"`
	At       time.Time    `json:"at"`
}

func cloneOperationsIn(tx *ledger.Tx) ([]CloneOperation, error) {
	raw, err := tx.Bindings(cloneKind)
	if err != nil {
		return nil, err
	}
	var out []CloneOperation
	for _, data := range raw {
		var op CloneOperation
		if err := json.Unmarshal(data, &op); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

func cloneBlocking(state string) bool { return state == "running" || state == "unconfirmed" }

func validateCloneOwnership(tx *ledger.Tx, desired map[string]Project) error {
	operations, err := cloneOperationsIn(tx)
	if err != nil {
		return err
	}
	for _, op := range operations {
		if !cloneBlocking(op.State) {
			continue
		}
		p, exists := desired[op.Project]
		copy, found := p.Copies[op.Copy.Node]
		if !exists || !found || !sameCopyDeclaration(copy, op.Copy) {
			return fmt.Errorf("%w: %s (%s:%s)", ErrCloneIsolated, op.ID, nodeLabel(op.Copy.Node), op.Copy.Path)
		}
		for _, other := range desired {
			for _, ws := range other.Workspaces() {
				if ws.ID != CopyID(op.Project, op.Copy.Node) && ws.Node == op.Copy.Node && pathsOverlap(ws.Path, op.Copy.Path) {
					return fmt.Errorf("%w: %s still owns %s", ErrCloneIsolated, op.ID, op.Copy.Path)
				}
			}
		}
	}
	return nil
}

func (s *Store) CloneOperations(ctx context.Context) ([]CloneOperation, error) {
	var out []CloneOperation
	err := s.l.Update(ctx, func(tx *ledger.Tx) error { var err error; out, err = cloneOperationsIn(tx); return err })
	return out, err
}

// BeginClone takes the same copy fence as interactive work, verifies the current
// declaration, and persists the physical ownership before any file operation.
func (s *Store) BeginClone(ctx context.Context, projectID string, copy Copy, region, source string, ttl time.Duration) (CloneOperation, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return CloneOperation{}, errors.New("clone operation needs a source attribution")
	}
	if err := s.checkDeclaration(ctx); err != nil {
		return CloneOperation{}, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return CloneOperation{}, err
	}
	id := "clone-" + hex.EncodeToString(token[:])
	lease, err := s.l.AcquireIn(ctx, region, CopyID(projectID, copy.Node), id, ttl)
	if err != nil {
		return CloneOperation{}, err
	}
	op := CloneOperation{ID: id, Project: projectID, Copy: copy, Lease: lease, State: "running", Source: source, At: s.now().UTC()}
	if _, err := s.l.Begin(ctx, id, cloneKind, "prepared", source, op); err != nil {
		_ = s.l.ReleaseAny(context.WithoutCancel(ctx), lease)
		return CloneOperation{}, err
	}
	_, err = s.l.Transition(ctx, id, "prepared", "running", source, []ledger.Lease{lease}, nil, func(tx *ledger.Tx, _ *ledger.Operation) error {
		all, err := projectsIn(tx)
		if err != nil {
			return err
		}
		p, exists := all[projectID]
		current, found := p.Copies[copy.Node]
		if !exists || !found || !sameCopyDeclaration(current, copy) || current.State != CopyProvisioning {
			return fmt.Errorf("%w: clone declaration changed", ErrUnknown)
		}
		operations, err := cloneOperationsIn(tx)
		if err != nil {
			return err
		}
		for _, active := range operations {
			if cloneBlocking(active.State) && active.Copy.Node == copy.Node && pathsOverlap(active.Copy.Path, copy.Path) {
				return fmt.Errorf("%w: %s", ErrCloneIsolated, active.ID)
			}
		}
		return tx.PutBinding(cloneKind, id, op)
	})
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.l.Transition(cleanup, id, "prepared", "failed", source, nil, map[string]string{"evidence": "file operation never dispatched", "error": err.Error()}, nil)
		_ = s.l.ReleaseAny(cleanup, lease)
		return CloneOperation{}, err
	}
	return op, nil
}

func (s *Store) RenewClone(ctx context.Context, op *CloneOperation, ttl time.Duration) error {
	next, err := s.l.RenewAny(ctx, op.Lease, ttl)
	if err != nil {
		return err
	}
	op.Lease = next
	_, err = s.l.Transition(ctx, op.ID, "running", "running", op.Source, []ledger.Lease{next}, nil, func(tx *ledger.Tx, current *ledger.Operation) error {
		if err := tx.SetData(current, *op); err != nil {
			return err
		}
		return tx.PutBinding(cloneKind, op.ID, *op)
	})
	return err
}

func (s *Store) finishClone(ctx context.Context, op CloneOperation, from, state, actor, evidence string, cause error) error {
	op.State, op.Evidence, op.At = state, evidence, s.now().UTC()
	if cause != nil {
		op.Error = cause.Error()
	}
	_, err := s.l.Transition(ctx, op.ID, from, state, actor, nil, map[string]string{"source": op.Source, "evidence": evidence}, func(tx *ledger.Tx, current *ledger.Operation) error {
		if err := tx.SetData(current, op); err != nil {
			return err
		}
		if err := tx.PutBinding(cloneKind, op.ID, op); err != nil {
			return err
		}
		all, err := projectsIn(tx)
		if err != nil {
			return err
		}
		p, exists := all[op.Project]
		copy, found := p.Copies[op.Copy.Node]
		if exists && found && sameCopyDeclaration(copy, op.Copy) {
			copy.State, copy.Error = CopyReady, ""
			if state != "succeeded" {
				copy.State, copy.Error = CopyFailed, op.Error
			}
			if state == "unconfirmed" {
				copy.Error = fmt.Sprintf("clone %s outcome unconfirmed; workspace isolated: %s", op.ID, op.Error)
			}
			p.Copies[copy.Node] = copy
			return tx.PutBinding(kindProject, p.ID, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("clone %s completion not recorded; workspace remains isolated: %w", op.ID, err)
	}
	if state != "unconfirmed" {
		if err := s.l.ReleaseAny(ctx, op.Lease); err != nil && !errors.Is(err, ledger.ErrStale) {
			return fmt.Errorf("clone %s stopped but lease release failed: %w", op.ID, err)
		}
	}
	return nil
}

// FinishClone records terminal evidence with a fresh cleanup context supplied by
// the caller. Uncertain remote completion never permits automatic replay/removal.
func (s *Store) FinishClone(ctx context.Context, op CloneOperation, confirmed bool, evidence string, cause error) error {
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return errors.New("clone completion needs outcome evidence")
	}
	state := "succeeded"
	if cause != nil {
		state = "failed"
	}
	if !confirmed {
		state = "unconfirmed"
	}
	return s.finishClone(ctx, op, "running", state, op.Source, evidence, cause)
}

// ConfirmCloneStopped is an explicit operator recovery action. Caller identity
// and concrete stop evidence are mandatory and retained in the operation journal.
func (s *Store) ConfirmCloneStopped(ctx context.Context, id, actor, evidence string) error {
	actor, evidence = strings.TrimSpace(actor), strings.TrimSpace(evidence)
	if actor == "" || evidence == "" {
		return errors.New("confirming a clone stopped requires an actor and evidence")
	}
	operations, err := s.CloneOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.ID == id {
			if !cloneBlocking(op.State) {
				return nil
			}
			return s.finishClone(ctx, op, op.State, "failed", actor, evidence, errors.New("operator confirmed the clone stopped"))
		}
	}
	return fmt.Errorf("clone operation %s not found", id)
}
