package contentreplica

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

func validID(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
func digest(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateScope(scope Scope) error {
	if !validID(scope.ProjectID) || !validID(scope.HomeNodeID) {
		return ErrInvalid
	}
	switch scope.Level {
	case "public", "internal", "restricted", "sealed":
	default:
		return ErrInvalid
	}
	return nil
}

func validateObject(object Object, limit int64) error {
	if err := validateScope(object.Scope); err != nil {
		return err
	}
	if !validID(object.Key) {
		return ErrInvalid
	}
	if !digest(object.Blob.SHA256, 64) || object.Blob.Size < 0 {
		return ErrInvalid
	}
	if object.Blob.Size > limit {
		return ErrTooLarge
	}
	switch object.Kind {
	case Material:
		if object.Key != object.Blob.SHA256 {
			return ErrInvalid
		}
	case GitBundle:
		if !digest(object.Key, 40) && !digest(object.Key, 64) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func placement(ctx context.Context, policy PlacementPolicy, scope Scope, node string) (Placement, error) {
	if err := ctx.Err(); err != nil {
		return Placement{}, err
	}
	if !validID(node) || scope.Level == "sealed" && node != scope.HomeNodeID {
		return Placement{}, ErrPlacement
	}
	p, err := policy.CheckpointPlacement(ctx, scope, node)
	if err != nil {
		return Placement{}, fmt.Errorf("%w: %w", ErrPlacement, err)
	}
	if !validID(p.FailureDomain) {
		return Placement{}, fmt.Errorf("%w: independent failure domain required", ErrPlacement)
	}
	return p, nil
}

func validateManifest(m Manifest, limit int64) error {
	if err := validateObject(m.Object, limit); err != nil {
		return err
	}
	if m.ID != m.Object.ID() || len(m.Receipts) > 256 {
		return ErrInvalid
	}
	switch m.Protection {
	case Replicated:
		if m.RequiredCopies != 2 || m.Object.Scope.Level == "sealed" {
			return ErrInvalid
		}
	case SingleNode:
		if m.RequiredCopies != 1 || m.Object.Scope.Level == "sealed" {
			return ErrInvalid
		}
	case SealedHome:
		if m.RequiredCopies != 1 || m.Object.Scope.Level != "sealed" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	nodes, domains := map[string]bool{}, map[string]bool{}
	for _, receipt := range m.Receipts {
		if receipt.ObjectID != m.ID || !validID(receipt.NodeID) || !validID(receipt.FailureDomain) || receipt.StoredAt.IsZero() || nodes[receipt.NodeID] {
			return ErrIntegrity
		}
		if m.Object.Scope.Level == "sealed" && receipt.NodeID != m.Object.Scope.HomeNodeID {
			return ErrPlacement
		}
		nodes[receipt.NodeID], domains[receipt.FailureDomain] = true, true
	}
	if len(domains) < m.RequiredCopies {
		return ErrIncomplete
	}
	return nil
}
