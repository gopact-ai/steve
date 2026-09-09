package plugins

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Selection is a session's fixed set of prepared node deployments. Ordering
// does not change identity; two revisions of one installation cannot mix.
type Selection struct {
	Project     string   `json:"project"`
	Node        string   `json:"node"`
	Harness     string   `json:"harness"`
	Deployments []string `json:"deployments"`
}

// RuntimeRef is safe to retain in the shared session/attempt ledger. The
// runtime record with actual machine configuration remains on the node.
type RuntimeRef struct {
	ID        string    `json:"id"`
	Selection Selection `json:"selection"`
}

func (selection Selection) Hash() (string, error) {
	if selection.Project == "" || len(selection.Project) > 256 || strings.ContainsAny(selection.Project+selection.Node, "\x00\r\n") || len(selection.Node) > 128 || !nameShape.MatchString(selection.Harness) || len(selection.Deployments) == 0 || len(selection.Deployments) > 64 || duplicate(selection.Deployments) {
		return "", fmt.Errorf("%w: runtime selection", ErrInvalid)
	}
	for _, digest := range selection.Deployments {
		if !digestShape.MatchString(digest) {
			return "", fmt.Errorf("%w: runtime deployment identity", ErrInvalid)
		}
	}
	selection.Deployments = slices.Clone(selection.Deployments)
	slices.Sort(selection.Deployments)
	raw, err := json.Marshal(selection)
	if err != nil {
		return "", err
	}
	return contentDigest(raw), nil
}

func (s *Store) Selection(selection Selection) ([]DeploymentReceipt, error) {
	if _, err := selection.Hash(); err != nil {
		return nil, err
	}
	receipts := make([]DeploymentReceipt, 0, len(selection.Deployments))
	seen := map[string]bool{}
	hashes := slices.Clone(selection.Deployments)
	slices.Sort(hashes)
	for _, hash := range hashes {
		receipt, err := s.Deployment(hash)
		if err != nil {
			return nil, err
		}
		deployment := receipt.Deployment
		if deployment.Node != selection.Node || !slices.Contains(deployment.Projects, selection.Project) || seen[deployment.Installation] {
			return nil, fmt.Errorf("%w: runtime selection differs from its deployment scope", ErrInvalid)
		}
		seen[deployment.Installation] = true
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func (ref RuntimeRef) Validate() error {
	if !digestShape.MatchString(ref.ID) {
		return ErrInvalid
	}
	_, err := ref.Selection.Hash()
	return err
}

func (ref *RuntimeRef) Clone() *RuntimeRef {
	if ref == nil {
		return nil
	}
	copied := *ref
	copied.Selection.Deployments = slices.Clone(ref.Selection.Deployments)
	return &copied
}
