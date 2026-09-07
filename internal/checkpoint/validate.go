package checkpoint

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
)

func validID(id string) bool {
	if id == "" || len(id) > 512 {
		return false
	}
	return !strings.ContainsAny(id, "\x00\r\n")
}

func validDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateScope(scope Scope) error {
	if !validID(scope.ProjectID) || !validID(scope.HomeNodeID) {
		return fmt.Errorf("%w: project and home node identities are required", ErrInvalid)
	}
	switch scope.Level {
	case "public", "internal", "restricted", "sealed":
		return nil
	default:
		return fmt.Errorf("%w: unknown data level", ErrInvalid)
	}
}

func validateBlob(ref BlobRef, limits Limits) error {
	if !validDigest(ref.SHA256) || ref.Size < 0 {
		return fmt.Errorf("%w: malformed blob reference", ErrInvalid)
	}
	if ref.Size > limits.MaxBlobBytes {
		return fmt.Errorf("%w: blob exceeds %d bytes", ErrQuota, limits.MaxBlobBytes)
	}
	return nil
}

func validateSource(source Source) error {
	if !validID(source.TaskID) || !validID(source.SessionID) || !validID(source.AttemptID) || !validID(source.TurnID) || !validID(source.NodeID) || source.ExecutionEpoch == 0 {
		return fmt.Errorf("%w: stable source identities and a writer epoch are required", ErrInvalid)
	}
	return nil
}

func validPath(name string) bool {
	if !fs.ValidPath(name) || name == "." || path.Clean(name) != name || strings.ContainsAny(name, "\\:\x00\r\n") || len(name) > 4096 {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		base, _, _ := strings.Cut(strings.ToUpper(part), ".")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return false
		}
	}
	return true
}

func canonical(snapshot Snapshot, limits Limits) (Snapshot, string, []BlobRef, error) {
	if snapshot.Version != 1 || snapshot.RequiredCopies < 1 || snapshot.RequiredCopies > 256 || snapshot.CreatedAt.IsZero() || !validID(snapshot.Workspace.ID) {
		return Snapshot{}, "", nil, fmt.Errorf("%w: malformed snapshot header", ErrInvalid)
	}
	if err := validateScope(snapshot.Scope); err != nil {
		return Snapshot{}, "", nil, err
	}
	if err := validateSource(snapshot.Source); err != nil {
		return Snapshot{}, "", nil, err
	}
	if len(snapshot.Workspace.Files)+len(snapshot.Materials) > limits.MaxFiles {
		return Snapshot{}, "", nil, fmt.Errorf("%w: too many files", ErrQuota)
	}
	if len(snapshot.UnknownActions) > limits.MaxFiles {
		return Snapshot{}, "", nil, fmt.Errorf("%w: too many uncertain actions", ErrQuota)
	}
	snapshot.Workspace.Files = slices.Clone(snapshot.Workspace.Files)
	snapshot.Materials = slices.Clone(snapshot.Materials)
	snapshot.UnknownActions = slices.Clone(snapshot.UnknownActions)
	snapshot.CreatedAt = snapshot.CreatedAt.UTC()
	refs := map[string]BlobRef{}
	total := int64(0)
	add := func(ref BlobRef) error {
		if err := validateBlob(ref, limits); err != nil {
			return err
		}
		if previous, ok := refs[ref.SHA256]; ok && previous.Size != ref.Size {
			return fmt.Errorf("%w: conflicting blob size", ErrInvalid)
		}
		if ref.Size > limits.MaxSnapshotBytes-total {
			return fmt.Errorf("%w: restored content exceeds %d bytes", ErrQuota, limits.MaxSnapshotBytes)
		}
		total += ref.Size
		refs[ref.SHA256] = ref
		return nil
	}
	if err := add(snapshot.Context); err != nil {
		return Snapshot{}, "", nil, err
	}
	for _, files := range [][]File{snapshot.Workspace.Files, snapshot.Materials} {
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		seen := map[string]bool{}
		for _, file := range files {
			name := strings.ToLower(file.Path)
			if !validPath(file.Path) || seen[name] {
				return Snapshot{}, "", nil, fmt.Errorf("%w: invalid or colliding file path", ErrInvalid)
			}
			seen[name] = true
			if err := add(file.Blob); err != nil {
				return Snapshot{}, "", nil, err
			}
		}
		for name := range seen {
			for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
				if seen[parent] {
					return Snapshot{}, "", nil, fmt.Errorf("%w: file/directory collision", ErrInvalid)
				}
			}
		}
	}
	sort.Slice(snapshot.UnknownActions, func(i, j int) bool { return snapshot.UnknownActions[i].ID < snapshot.UnknownActions[j].ID })
	for i, action := range snapshot.UnknownActions {
		if !validID(action.ID) || strings.TrimSpace(action.Description) == "" || len(action.Description) > 8192 || len(action.ReconcileRef) > 4096 || (i > 0 && snapshot.UnknownActions[i-1].ID == action.ID) {
			return Snapshot{}, "", nil, fmt.Errorf("%w: malformed external action", ErrInvalid)
		}
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return Snapshot{}, "", nil, err
	}
	if int64(len(raw)) > limits.MaxManifestBytes {
		return Snapshot{}, "", nil, fmt.Errorf("%w: manifest exceeds %d bytes", ErrQuota, limits.MaxManifestBytes)
	}
	blobs := make([]BlobRef, 0, len(refs))
	for _, ref := range refs {
		blobs = append(blobs, ref)
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].SHA256 < blobs[j].SHA256 })
	return snapshot, Reference(raw).SHA256, blobs, nil
}

func completeManifest(snapshot Snapshot, receipts []Receipt, limits Limits) (Manifest, error) {
	if len(receipts) > 256 {
		return Manifest{}, fmt.Errorf("%w: too many replica receipts", ErrInvalid)
	}
	canonicalSnapshot, id, _, err := canonical(snapshot, limits)
	if err != nil {
		return Manifest{}, err
	}
	receipts = slices.Clone(receipts)
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].NodeID < receipts[j].NodeID })
	nodes, domains := map[string]bool{}, map[string]bool{}
	for _, receipt := range receipts {
		if receipt.SnapshotID != id || !validID(receipt.NodeID) || !validID(receipt.FailureDomain) || receipt.StoredAt.IsZero() || nodes[receipt.NodeID] {
			return Manifest{}, fmt.Errorf("%w: malformed replica receipt", ErrInvalid)
		}
		nodes[receipt.NodeID], domains[receipt.FailureDomain] = true, true
	}
	if len(domains) < canonicalSnapshot.RequiredCopies {
		return Manifest{}, fmt.Errorf("%w: need %d independent copies, have %d", ErrIncomplete, canonicalSnapshot.RequiredCopies, len(domains))
	}
	m := Manifest{Snapshot: canonicalSnapshot, Receipts: receipts}
	raw, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, err
	}
	if int64(len(raw)) > limits.MaxManifestBytes {
		return Manifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrQuota, limits.MaxManifestBytes)
	}
	m.ID = Reference(raw).SHA256
	return m, nil
}

func validateManifest(m Manifest, limits Limits) (Manifest, error) {
	canonicalManifest, err := completeManifest(m.Snapshot, m.Receipts, limits)
	if err != nil {
		return Manifest{}, err
	}
	if canonicalManifest.ID != m.ID {
		return Manifest{}, fmt.Errorf("%w: manifest digest mismatch", ErrIntegrity)
	}
	return canonicalManifest, nil
}

func cloneManifest(m Manifest) Manifest {
	m.Receipts = slices.Clone(m.Receipts)
	m.Snapshot.Workspace.Files = slices.Clone(m.Snapshot.Workspace.Files)
	m.Snapshot.Materials = slices.Clone(m.Snapshot.Materials)
	m.Snapshot.UnknownActions = slices.Clone(m.Snapshot.UnknownActions)
	return m
}
