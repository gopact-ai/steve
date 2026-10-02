package attempt

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

type RecoveryArtifactCheck func(ledger.Reader, string, string, string) (RecoveryContent, error)

func sameRecoveryWorkspace(a, b project.Workspace) bool {
	return a.ID == b.ID && a.Project == b.Project && a.Node == b.Node && samePhysicalPath(a.Path, b.Path) && a.Kind == project.KindWorktree && b.Kind == project.KindWorktree && a.RecoveryID == b.RecoveryID
}

func checkRecoveryExecutionTx(tx ledger.Reader, spec Spec) (WorkspaceRecovery, error) {
	if spec.WorkspaceRecovery == nil || spec.WorkspaceRecovery.ID != spec.Workspace.RecoveryID || spec.Execution == nil {
		return WorkspaceRecovery{}, ErrWorkspaceRecovery
	}
	r, err := recoveryByIDTx(tx, spec.WorkspaceRecovery.ID)
	if err != nil {
		return r, err
	}
	if err := checkRecoveryDeclarationTx(tx, r); err != nil {
		return r, err
	}
	if !sameRecoveryWorkspace(r.Workspace, spec.Workspace) || r.Workspace.Node != spec.Node || r.Phase != "ready" || r.Producer != nil {
		return r, ErrWorkspaceRecovery
	}
	for _, source := range r.Sources {
		original, err := GetTx(tx, source.Attempt)
		if err != nil {
			return r, err
		}
		if original.Abandoned == nil || original.Abandoned.WorkspaceRecoveryID != r.ID || original.Abandoned.ProjectedAt.IsZero() {
			return r, ErrWorkspaceRecovery
		}
	}
	return r, nil
}

// The workspace lease is already owned here. The serialized attempt is then
// checked again together with its durable producer reservation at Begin.
func (s *Service) recoveryBaseAfterLease(ctx context.Context, spec *Spec) error {
	if spec.Workspace.RecoveryID == "" && spec.WorkspaceRecovery == nil {
		return nil
	}
	return s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		r, err := checkRecoveryExecutionTx(tx, *spec)
		if err != nil {
			return err
		}
		spec.Base, spec.Workspace.Base = r.Head.Artifact, r.Head.Artifact
		spec.WorkspaceRecovery = &RecoveryExecution{ID: r.ID, HeadVersion: r.Head.Version}
		return nil
	})
}

func reserveRecoveryProducerTx(tx *ledger.Tx, spec Spec) error {
	if spec.Workspace.RecoveryID == "" && spec.WorkspaceRecovery == nil {
		return nil
	}
	r, err := checkRecoveryExecutionTx(tx, spec)
	if err != nil {
		return err
	}
	if r.Head.Artifact != spec.Base || r.Head.Version != spec.WorkspaceRecovery.HeadVersion {
		return ledger.ErrConflict
	}
	nativeMayWrite := false
	r.Producer = &RecoveryProducer{NativeMayWrite: &nativeMayWrite, Attempt: spec.ID, Execution: *spec.Execution, Base: spec.Base, HeadVersion: r.Head.Version}
	r.Phase = "working"
	return saveWorkspaceRecoveryTx(tx, &r, spec.By)
}

func checkRecoveryWriterTx(tx ledger.Reader, spec Spec) error {
	all, err := workspaceRecoveriesTx(tx)
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Workspace.Path == "" || r.Workspace.Node != spec.Workspace.Node || !samePhysicalPath(r.Workspace.Path, spec.Workspace.Path) {
			continue
		}
		if spec.WorkspaceRecovery == nil || spec.WorkspaceRecovery.ID != r.ID || !sameRecoveryWorkspace(spec.Workspace, r.Workspace) {
			return ErrWorkspaceRecovery
		}
		if r.Producer != nil && r.Producer.Attempt != spec.ID {
			producer, err := GetTx(tx, r.Producer.Attempt)
			if err != nil {
				return err
			}
			if !producer.State.Terminal() && !producer.Unsettled {
				return Busy{Resource: "workspace:" + r.Workspace.ID, Holder: producer.ID}
			}
			return ErrWorkspaceRecovery
		}
		if r.Producer != nil && (spec.Execution == nil || *spec.Execution != r.Producer.Execution || spec.Base != r.Producer.Base || spec.WorkspaceRecovery.HeadVersion != r.Producer.HeadVersion) {
			return ErrWorkspaceRecovery
		}
		if err := checkRecoveryDeclarationTx(tx, r); err != nil {
			return err
		}
		if r.Phase != "ready" && r.Phase != "working" {
			return ErrWorkspaceRecovery
		}
	}
	return nil
}

func completeWorkspaceRecoveryTx(tx *ledger.Tx, record Record, binding *NameBinding, validate RecoveryArtifactCheck) error {
	if record.WorkspaceRecovery == nil {
		return nil
	}
	r, err := recoveryByIDTx(tx, record.WorkspaceRecovery.ID)
	if err != nil {
		return err
	}
	if r.Phase != "working" || r.Producer == nil || r.Producer.Attempt != record.ID || record.Execution == nil || r.Producer.Execution != *record.Execution || record.Base != r.Head.Artifact || record.WorkspaceRecovery.HeadVersion != r.Head.Version {
		return ErrWorkspaceRecovery
	}
	if err := checkRecoveryDeclarationTx(tx, r); err != nil {
		return err
	}
	if err := task.CheckExecutionTx(tx, record.Execution); err != nil {
		return err
	}
	if record.Result == nil || record.Result.CaptureError != "" || record.Result.RecoveryOutput == nil || validate == nil {
		return errors.New("recovery output has not been durably prepared")
	}
	artifact := record.Result.Artifact
	proposal := record.Result.RecoveryOutput
	if artifact != "" {
		if binding == nil || proposal.Name != binding.Name || proposal.ExpectedVersion != binding.ExpectedVersion {
			return errors.New("recovery output differs from its named completion")
		}
	} else {
		if binding != nil || proposal.Name != "" {
			return errors.New("unchanged recovery output cannot move a result name")
		}
		artifact = r.Head.Artifact
	}
	content, err := validate(tx, r.Project, r.Head.Artifact, artifact)
	if err != nil {
		return err
	}
	if record.Result.Artifact != "" {
		r.Head.Artifact = artifact
		r.Head.ContentID, r.Head.Storage, r.Head.Evidence = content.ID, content.Storage, content.Evidence
		r.Head.Version++
		producer := *r.Producer
		producer.Artifact, producer.ContentID, producer.Storage, producer.Evidence = artifact, content.ID, content.Storage, content.Evidence
		r.Head.Sources = append(r.Head.Sources, producer)
	}
	r.Producer = nil
	r.Phase = "ready"
	return saveWorkspaceRecoveryTx(tx, &r, "recovery-output")
}
