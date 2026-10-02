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
