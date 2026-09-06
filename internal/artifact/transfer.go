package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

type ProjectTransfer struct {
	Project   string               `json:"project"`
	Facts     ledger.TransferFacts `json:"facts"`
	GitBundle []byte               `json:"git_bundle"`
}

func (s *Store) ExportProject(ctx context.Context, project string) (ProjectTransfer, error) {
	out := ProjectTransfer{Project: project, Facts: ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{}}}
	artifacts := map[string]bool{}
	for _, kind := range []string{manifestKind, attestationKind, pendingKind} {
		raw, err := s.ledger.Bindings(ctx, kind)
		if err != nil {
			return out, err
		}
		selected := map[string]json.RawMessage{}
		for id, value := range raw {
			var marker struct {
				Project string `json:"project"`
			}
			if err := json.Unmarshal(value, &marker); err != nil {
				return out, err
			}
			if marker.Project == project {
				selected[id] = value
				if kind == manifestKind {
					artifacts[id] = true
				}
			}
		}
		out.Facts.Bindings[kind] = selected
	}
	replicas, err := s.ledger.Bindings(ctx, replicaKind)
	if err != nil {
		return out, err
	}
	out.Facts.Bindings[replicaKind] = map[string]json.RawMessage{}
	for id, raw := range replicas {
		var r Replica
		if err := json.Unmarshal(raw, &r); err != nil {
			return out, err
		}
		if artifacts[r.Artifact] {
			out.Facts.Bindings[replicaKind][id] = raw
		}
	}
	names, err := s.ledger.Names(ctx, "")
	if err != nil {
		return out, err
	}
	for _, name := range names {
		if artifacts[name.Artifact] {
			out.Facts.Names = append(out.Facts.Names, name)
		}
	}
	ops, err := s.ledger.Operations(ctx, landKind, "")
	if err != nil {
		return out, err
	}
	var ids []string
	for _, op := range ops {
		var land Landing
		if err := json.Unmarshal(op.Data, &land); err != nil {
			return out, err
		}
		if land.Project == project {
			if op.State == LandApplying || op.State == LandRecoveryPending {
				return out, fmt.Errorf("landing %s has unresolved file effects", op.ID)
			}
			ids = append(ids, op.ID)
		}
	}
	facts, err := s.ledger.ExportOperations(ctx, ids)
	if err != nil {
		return out, err
	}
	out.Facts.Add(facts)
	repo := filepath.Join(s.Dir, "objects", project+".git")
	if _, err := os.Stat(repo); os.IsNotExist(err) {
		if len(artifacts) > 0 {
			return out, fmt.Errorf("project artifact objects unavailable locally")
		}
		return out, nil
	} else if err != nil {
		return out, err
	}
	path := filepath.Join(os.TempDir(), "unused")
	tmp, err := os.CreateTemp("", "steve-artifact-*.bundle")
	if err != nil {
		return out, err
	}
	path = tmp.Name()
	tmp.Close()
	os.Remove(path)
	defer os.Remove(path)
	cmd := exec.CommandContext(ctx, "git", "--git-dir", repo, "bundle", "create", path, "--all")
	if raw, err := cmd.CombinedOutput(); err != nil {
		if len(artifacts) == 0 && strings.Contains(string(raw), "empty bundle") {
			return out, nil
		}
		return out, fmt.Errorf("bundle project history: %w: %s", err, raw)
	}
	out.GitBundle, err = os.ReadFile(path)
	return out, err
}
func ImportProjectObjects(ctx context.Context, dir string, in ProjectTransfer, retry bool) error {
	if len(in.GitBundle) == 0 {
		return nil
	}
	if in.Project == "" || strings.ContainsAny(in.Project, "/\\") {
		return fmt.Errorf("invalid artifact project")
	}
	repo := filepath.Join(dir, "objects", in.Project+".git")
	if _, err := os.Stat(repo); err == nil && !retry {
		return fmt.Errorf("artifact repository already exists: %s", in.Project)
	}
	tmp, err := os.CreateTemp("", "steve-import-*.bundle")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(in.GitBundle); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	r, err := Open(ctx, repo)
	if err != nil {
		return err
	}
	if err := r.Unbundle(ctx, path); err != nil {
		return err
	}
	for id := range in.Facts.Bindings[manifestKind] {
		if !r.Has(ctx, id) {
			return fmt.Errorf("artifact bundle does not contain commit %s", id)
		}
	}
	return nil
}

func (in *ProjectTransfer) Remap(m ledger.TransferIDs, target project.Home) error {
	for i := range in.Facts.Operations {
		op := &in.Facts.Operations[i]
		var land Landing
		if err := json.Unmarshal(op.Data, &land); err != nil {
			return err
		}
		land.Lease = nil
		land.ID = m.Operation(land.ID)
		if land.Source != nil && land.Source.Execution != nil {
			land.Source.Execution.TaskID = m.Task(land.Source.Execution.TaskID)
		}
		if land.Recoverable {
			land.Target = target
		}
		op.Data, _ = json.Marshal(land)
	}
	for _, kind := range []string{attestationKind, pendingKind} {
		for id, raw := range in.Facts.Bindings[kind] {
			if kind == attestationKind {
				var a Attestation
				if err := json.Unmarshal(raw, &a); err != nil {
					return err
				}
				a.TaskID = m.Task(a.TaskID)
				raw, _ = json.Marshal(a)
			} else {
				var p Pending
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if p.Source != nil && p.Source.Execution != nil {
					p.Source.Execution.TaskID = m.Task(p.Source.Execution.TaskID)
				}
				raw, _ = json.Marshal(p)
			}
			in.Facts.Bindings[kind][id] = raw
		}
	}
	for id, raw := range in.Facts.Bindings[replicaKind] {
		var r Replica
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		r.Note = fmt.Sprintf("source replica %s generation %d; verify in destination node partition", r.State, r.Generation)
		r.State = ReplicaQuarantined
		r.Generation = 0
		in.Facts.Bindings[replicaKind][id], _ = json.Marshal(r)
	}
	in.Facts.RemapEnvelopes(m)
	return nil
}
