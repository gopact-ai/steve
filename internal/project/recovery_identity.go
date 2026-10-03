package project

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/workspacepath"
)

// ReadTx keeps declaration data in the same snapshot as a domain decision.
func ReadTx(tx ledger.Reader, id string) (Project, error) {
	var raw []byte
	if err := tx.QueryRow("SELECT data FROM bindings WHERE kind=? AND id=?", kindProject, id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, fmt.Errorf("%w: %s", ErrUnknown, id)
		}
		return Project{}, err
	}
	var p Project
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if p.ID != id || p.Home.Path == "" {
		return Project{}, errors.New("project declaration identity differs")
	}
	return p.normalized()
}

// RecoveryIdentity fixes placement and data handling, not grants or instructions.
// Current authorization must still be checked whenever new work is requested.
// The digest uses slash-lexical path.Clean; directory equality is a separate
// metadata comparison.
func RecoveryIdentity(p Project) string {
	p.Copies = maps.Clone(p.Copies)
	p.DurablePlaces = slices.Clone(p.DurablePlaces)
	normalized, err := p.normalized()
	if err != nil {
		return ""
	}
	p = normalized
	places := slices.Clone(p.DurablePlaces)
	slices.Sort(places)
	raw, _ := json.Marshal(struct {
		ID      string
		Home    Home
		Level   string
		Repo    RepoMode
		Durable []string
	}{p.ID, Home{Node: p.Home.Node, Path: path.Clean(p.Home.Path)}, string(p.Level.OrDefault()), p.Repo, places})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CheckRecoveryWorkspaceTx keeps an isolated recovery container separate from
// every current project declaration in the transaction that fixes its location.
func CheckRecoveryWorkspaceTx(tx ledger.Reader, workspace Workspace) error {
	if workspace.Kind != KindWorktree || workspace.RecoveryID == "" || !workspacepath.IsAbs(workspace.Path) || workspacepath.Base(workspace.Path) != "work" {
		return errors.New("recovery workspace has no exact isolated container")
	}
	last := ""
	for {
		var id string
		err := tx.QueryRow(`SELECT id FROM bindings WHERE kind=? AND id>? ORDER BY id LIMIT 1`, kindProject, last).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := ReadTx(tx, id)
		if err != nil {
			return err
		}
		p, err = p.normalized()
		if err != nil {
			return err
		}
		for _, place := range p.Workspaces() {
			if place.Node == workspace.Node && workspacepath.Overlap(place.Path, workspacepath.Dir(workspace.Path)) {
				return fmt.Errorf("recovery container overlaps project %s on %s", p.ID, nodeLabel(workspace.Node))
			}
		}
		last = id
	}
}

// ReadHistoricalTx is the read-only artifact port for accepted historical data.
func ReadHistoricalTx(tx ledger.Reader, id string) (Project, error) {
	p, err := ReadTx(tx, id)
	if err == nil || !errors.Is(err, ErrUnknown) {
		return p, err
	}
	var raw string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, retiredKind, id).Scan(&raw); err != nil {
		return Project{}, err
	}
	var retired retiredProject
	if json.Unmarshal([]byte(raw), &retired) != nil || retired.Project.ID != id {
		return Project{}, errors.New("retired project identity differs")
	}
	return retired.Project.normalized()
}
