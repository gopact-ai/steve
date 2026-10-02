package contentreplica

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
)

const GitStorageEvidenceKind = "git-storage-evidence"

// GitStorageEvidence is an immutable artifact-owner fact, not execution
// authority. Its digest detects inconsistent references and individual rows.
// Historical evidence never grants current project or producer permission.
type GitStorageEvidence struct {
	Project   string `json:"project"`
	Artifact  string `json:"artifact"`
	Storage   string `json:"storage"`
	ContentID string `json:"content_id,omitempty"`
	Level     string `json:"level"`
	HomeNode  string `json:"home_node"`
}

func (e GitStorageEvidence) ID() string {
	raw, _ := json.Marshal(e)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (e GitStorageEvidence) Validate() error {
	if e.Project == "" || !digest(e.Artifact, 40) && !digest(e.Artifact, 64) {
		return ErrIntegrity
	}
	switch e.Level {
	case "public", "internal", "restricted", "sealed":
	default:
		return ErrIntegrity
	}
	switch e.Storage {
	case "replicated":
		if !digest(e.ContentID, 64) {
			return ErrIntegrity
		}
	case "standalone":
		if e.ContentID != "" {
			return ErrIntegrity
		}
	case "sealed-home":
		if e.ContentID != "" || e.Level != "sealed" || e.HomeNode == "" {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func LookupGitStorageEvidence(tx ledger.Reader, id string) (GitStorageEvidence, error) {
	var e GitStorageEvidence
	if !digest(id, 64) {
		return e, ErrIntegrity
	}
	var raw string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, GitStorageEvidenceKind, id).Scan(&raw); err != nil {
		return e, fmt.Errorf("%w: missing Git storage evidence %s", ErrIntegrity, id)
	}
	if json.Unmarshal([]byte(raw), &e) != nil || e.Validate() != nil || e.ID() != id {
		return e, ErrIntegrity
	}
	return e, nil
}
