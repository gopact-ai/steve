package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

const ownerKind = "project-owner"

var ErrNotOwner = errors.New("project is not active on this hub")
var ErrTransferPending = errors.New("project import must be completed before this hub can start")

type Ownership struct {
	Project    string    `json:"project"`
	HubID      string    `json:"hub_id"`
	Epoch      uint64    `json:"epoch"`
	State      string    `json:"state"`
	TransferID string    `json:"transfer_id,omitempty"`
	TargetHub  string    `json:"target_hub,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (s *Store) SetHubID(id string) { id = strings.TrimSpace(id); s.hubID.Store(&id) }
func (s *Store) HubID() string {
	if p := s.hubID.Load(); p != nil {
		return *p
	}
	return ""
}
func (s *Store) Ownership(ctx context.Context, id string) (Ownership, bool, error) {
	var o Ownership
	ok, err := s.l.GetBinding(ctx, ownerKind, id, &o)
	return o, ok, err
}

func (s *Store) checkOwner(ctx context.Context, id string) error {
	o, ok, err := s.Ownership(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if o.State != "active" || o.HubID != s.HubID() {
		return fmt.Errorf("%w: %s belongs to %s at epoch %d (%s)", ErrNotOwner, id, o.HubID, o.Epoch, o.State)
	}
	return nil
}

func (s *Store) guardOwners(tx *ledger.Tx, desired map[string]Project) error {
	if s.HubID() == "" {
		return nil
	}
	owners, err := tx.Bindings(ownerKind)
	if err != nil {
		return err
	}
	for id, raw := range owners {
		var o Ownership
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		if o.State == "importing" {
			return fmt.Errorf("%w: project %s, transfer %s; rerun the original import", ErrTransferPending, id, o.TransferID)
		}
	}
	existing, err := projectsIn(tx)
	if err != nil {
		return err
	}
	for id, p := range desired {
		raw, ok := owners[id]
		if !ok {
			if err := tx.PutBinding(ownerKind, id, Ownership{Project: id, HubID: s.HubID(), Epoch: 1, State: "active", UpdatedAt: s.now().UTC()}); err != nil {
				return err
			}
			continue
		}
		var o Ownership
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		if o.State == "active" && o.HubID == s.HubID() {
			continue
		}
		before, had := existing[id]
		if !had || !sameProjectDeclaration(before, p) {
			return fmt.Errorf("%w: configured declaration cannot change released/foreign project %s", ErrNotOwner, id)
		}
	}
	return nil
}

func sameProjectDeclaration(a, b Project) bool {
	if len(a.Copies) != len(b.Copies) {
		return false
	}
	for node, copy := range a.Copies {
		other, ok := b.Copies[node]
		if !ok || !sameCopyDeclaration(copy, other) {
			return false
		}
	}
	a.Copies, b.Copies = nil, nil
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// ActivateTransfer follows the durable configuration write. A crash before
// this transition keeps startup gated and the exact import can complete it.
func (s *Store) ActivateTransfer(ctx context.Context, id, transferID string, epoch uint64) error {
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		owners, err := tx.Bindings(ownerKind)
		if err != nil {
			return err
		}
		var o Ownership
		if err := json.Unmarshal(owners[id], &o); err != nil {
			return err
		}
		if o.HubID != s.HubID() || o.TransferID != transferID || o.Epoch != epoch || (o.State != "importing" && o.State != "active") {
			return errors.New("transfer activation identity mismatch")
		}
		if o.State == "active" {
			return nil
		}
		o.State, o.UpdatedAt = "active", s.now().UTC()
		return tx.PutBinding(ownerKind, id, o)
	})
}

// Release is called only in maintenance mode after the caller acquires this
// hub's process lock. guard must prove no possible project writer remains in
// the same transaction; an unverified process stop is not such proof.
func (s *Store) Release(ctx context.Context, id, target, transferID, evidence string, guard func(*ledger.Tx, string) error) (Ownership, error) {
	var released Ownership
	if s.HubID() == "" || target == "" || target == s.HubID() || transferID == "" || strings.TrimSpace(evidence) == "" || guard == nil {
		return released, errors.New("release requires source/target hub, transfer id, stop evidence and quiescence guard")
	}
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		raw, err := tx.Bindings(ownerKind)
		if err != nil {
			return err
		}
		if data, ok := raw[id]; ok {
			if err := json.Unmarshal(data, &released); err != nil {
				return err
			}
		} else {
			released = Ownership{Project: id, HubID: s.HubID(), Epoch: 1, State: "active"}
		}
		if released.State == "released" && released.TransferID == transferID && released.TargetHub == target {
			return nil
		}
		if released.HubID != s.HubID() || released.State != "active" {
			return ErrNotOwner
		}
		if err := guard(tx, id); err != nil {
			return err
		}
		projects, err := projectsIn(tx)
		if err != nil {
			return err
		}
		if _, ok := projects[id]; !ok {
			return ErrUnknown
		}
		released.Epoch++
		released.State = "released"
		released.TransferID = transferID
		released.TargetHub = target
		released.Evidence = evidence
		released.UpdatedAt = s.now().UTC()
		return tx.PutBinding(ownerKind, id, released)
	})
	return released, err
}

// Accept activates only the hub explicitly named by a released owner. It is
// intended for a validated offline import; another project's data never gets
// overwritten to make an import fit. Same-transfer replay is idempotent.
func (s *Store) Accept(ctx context.Context, p Project, released Ownership) error {
	if released.State != "released" || released.Project != p.ID || released.TargetHub != s.HubID() || released.TransferID == "" || released.Epoch < 2 || released.HubID == s.HubID() {
		return errors.New("invalid project transfer authority")
	}
	normalized, err := p.normalized()
	if err != nil {
		return err
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		owners, err := tx.Bindings(ownerKind)
		if err != nil {
			return err
		}
		all, err := projectsIn(tx)
		if err != nil {
			return err
		}
		if raw, found := owners[p.ID]; found {
			var o Ownership
			if err := json.Unmarshal(raw, &o); err != nil {
				return err
			}
			if o.HubID == s.HubID() && o.State == "active" && o.Epoch == released.Epoch && o.TransferID == released.TransferID {
				old, exists := all[p.ID]
				a, _ := json.Marshal(old)
				b, _ := json.Marshal(normalized)
				if !exists || string(a) != string(b) {
					return errors.New("same transfer cannot replace accepted project metadata")
				}
				return nil
			}
			return fmt.Errorf("project ownership collision or stale transfer: %s", p.ID)
		}
		if _, exists := all[p.ID]; exists {
			return fmt.Errorf("project %s already exists", p.ID)
		}
		all[p.ID] = normalized
		if err := validateOwnership(all); err != nil {
			return err
		}
		// Domain collision checks remain active; ownership is established here,
		// not by configured declaration's implicit first-owner assignment.
		for _, guard := range s.guards {
			values := make([]Project, 0, len(all))
			for _, v := range all {
				values = append(values, v)
			}
			if err := guard(tx, values); err != nil {
				return err
			}
		}
		accepted := released
		accepted.HubID = s.HubID()
		accepted.State = "active"
		accepted.UpdatedAt = s.now().UTC()
		if err := tx.PutBinding(kindProject, p.ID, normalized); err != nil {
			return err
		}
		return tx.PutBinding(ownerKind, p.ID, accepted)
	})
}

// Lookup reads retained project metadata without granting execution authority.
func (s *Store) Lookup(ctx context.Context, id string) (Project, bool, error) {
	return s.GetHistorical(ctx, id)
}

func (s *Store) ListOwnership(ctx context.Context) ([]Ownership, error) {
	raw, err := s.l.Bindings(ctx, ownerKind)
	if err != nil {
		return nil, err
	}
	out := make([]Ownership, 0, len(raw))
	for _, value := range raw {
		var o Ownership
		if err := json.Unmarshal(value, &o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}
