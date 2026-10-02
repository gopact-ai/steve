package attempt

import (
	"encoding/hex"
	"errors"
	"path"
	"strings"

	"github.com/gopact-ai/steve/internal/project"
)

func recoveryArtifactID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && (len(decoded) == 20 || len(decoded) == 32) && hex.EncodeToString(decoded) == id
}

func validateRecoveryContent(artifact, content, storage string) error {
	if !recoveryArtifactID(artifact) {
		return errors.New("recovery artifact identity is invalid")
	}
	switch storage {
	case "replicated":
		if content == "" {
			return errors.New("recovery replicated content identity is missing")
		}
	case "standalone", "sealed-home":
		if content != "" {
			return errors.New("recovery content location differs from its storage mode")
		}
	default:
		return errors.New("recovery artifact storage mode is unknown")
	}
	return nil
}

func validateWorkspaceRecovery(r WorkspaceRecovery) error {
	identity, err := hex.DecodeString(strings.TrimPrefix(r.ID, "workspace-recovery-"))
	if err != nil || len(identity) != 16 || r.ID != "workspace-recovery-"+hex.EncodeToString(identity) || r.CreatedAt.IsZero() || r.RequestedBy == "" || !path.IsAbs(r.Target.Path) || r.Baseline.Name != "project/"+r.Project+"/canonical" {
		return errors.New("workspace recovery source identity is invalid")
	}
	if err := validateRecoveryContent(r.Baseline.Artifact, r.Baseline.ContentID, r.Baseline.Storage); err != nil {
		return err
	}
	if err := validateRecoveryContent(r.Head.Artifact, r.Head.ContentID, r.Head.Storage); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, s := range r.Sources {
		if s.Attempt == "" || s.Task == "" || s.Revision == 0 || s.At.IsZero() || seen[s.Attempt] {
			return errors.New("workspace recovery has invalid original writer identity")
		}
		seen[s.Attempt] = true
	}
	if r.Phase != "recorded" && (r.Workspace.ID == "" || r.Workspace.RecoveryID != r.ID || r.Workspace.Project != r.Project || r.Workspace.Kind != project.KindWorktree || !path.IsAbs(r.Workspace.Path) || path.Base(r.Workspace.Path) != "work" || r.Workspace.Base != r.Baseline.Artifact) {
		return errors.New("workspace recovery has no exact prepared location")
	}
	for _, producer := range r.Head.Sources {
		if producer.Attempt == "" || producer.Execution.TaskID == "" || producer.Execution.Epoch == 0 || producer.HeadVersion < 1 {
			return errors.New("recovery head has no producing authority")
		}
		if err := validateRecoveryContent(producer.Artifact, producer.ContentID, producer.Storage); err != nil {
			return err
		}
	}
	if (r.Phase == "working") != (r.Producer != nil) {
		return errors.New("recovery writer obligation differs from phase")
	}
	if r.Producer != nil && (r.Producer.NativeMayWrite == nil || r.Producer.Attempt == "" || r.Producer.Execution.TaskID == "" || r.Producer.Execution.Epoch == 0 || r.Producer.Base != r.Head.Artifact || r.Producer.HeadVersion != r.Head.Version) {
		return errors.New("recovery writer obligation has no exact authority")
	}
	return nil
}
