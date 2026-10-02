package project

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
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
	return p, nil
}

// RecoveryIdentity fixes placement and data handling, not grants or instructions.
// Current authorization must still be checked whenever new work is requested.
func RecoveryIdentity(p Project) string {
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
	if workspace.Kind != KindWorktree || workspace.RecoveryID == "" || !path.IsAbs(workspace.Path) || path.Base(workspace.Path) != "work" {
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
			if place.Node == workspace.Node && pathsOverlap(place.Path, path.Dir(workspace.Path)) {
				return fmt.Errorf("recovery container overlaps project %s on %s", p.ID, nodeLabel(workspace.Node))
			}
		}
		last = id
	}
}
