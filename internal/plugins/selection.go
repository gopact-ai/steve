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
	ExcludedSkills []string                    `json:"excluded_skills,omitempty"`
	Filters        map[string]CapabilityFilter `json:"filters,omitempty"`
	Project        string                      `json:"project"`
	Node           string                      `json:"node"`
	Harness        string                      `json:"harness"`
	Deployments    []string                    `json:"deployments"`
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
	if err := selection.validateFilters(); err != nil {
		return "", err
	}
	selection = selection.Clone()
	slices.Sort(selection.ExcludedSkills)
	for id, filter := range selection.Filters {
		slices.Sort(filter.Skills)
		slices.Sort(filter.MCP)
		selection.Filters[id] = filter
	}
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
		if filter, limited := selection.Filters[deployment.Installation]; limited {
			bundle, err := s.Read(deployment.Digest)
			if err != nil {
				return nil, err
			}
			for _, name := range filter.Skills {
				if _, ok := bundle.Manifest.Skills[name]; !ok {
					return nil, fmt.Errorf("%w: preset skill %s is absent from %s; apply a compatible preset", ErrInvalid, name, bundle.Manifest.ID)
				}
			}
			for _, name := range filter.MCP {
				if _, ok := bundle.Manifest.MCP[name]; !ok {
					return nil, fmt.Errorf("%w: preset MCP %s is absent from %s; apply a compatible preset", ErrInvalid, name, bundle.Manifest.ID)
				}
			}
		}
		seen[deployment.Installation] = true
		receipts = append(receipts, receipt)
	}
	for id := range selection.Filters {
		if !seen[id] {
			return nil, ErrInvalid
		}
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
	copied.Selection = ref.Selection.Clone()
	return &copied
}
