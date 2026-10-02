package attempt

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

type recoveryEnvelope struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
	Data     string `json:"data"`
}

func decodeWorkspaceRecovery(e recoveryEnvelope) (WorkspaceRecovery, error) {
	var r WorkspaceRecovery
	if err := json.Unmarshal([]byte(e.Data), &r); err != nil {
		return r, err
	}
	if r.ID != e.ID || r.Revision != e.Revision || r.Phase != e.State || r.Revision < 1 || r.Project == "" || r.Target.Path == "" || r.Baseline.Artifact == "" || r.Baseline.Version < 1 || r.Head.Artifact == "" || r.Head.Version < 1 || len(r.Sources) == 0 || r.Declaration == "" {
		return r, errors.New("workspace recovery identity is incomplete or changed")
	}
	switch r.Phase {
	case "recorded", "materializing", "ready", "working", "draining", "capture", "landing":
	default:
		return r, errors.New("workspace recovery phase is invalid")
	}
	if err := validateWorkspaceRecovery(r); err != nil {
		return r, err
	}
	return r, nil
}

func recoveryByIDTx(tx ledger.Reader, id string) (WorkspaceRecovery, error) {
	var e recoveryEnvelope
	err := tx.QueryRow(`SELECT id,state,revision,data FROM operations WHERE kind=? AND id=?`, workspaceRecoveryKind, id).Scan(&e.ID, &e.State, &e.Revision, &e.Data)
	if err != nil {
		return WorkspaceRecovery{}, err
	}
	r, err := decodeWorkspaceRecovery(e)
	if err == nil {
		err = validateWorkspaceRecoveryTx(tx, r)
	}
	return r, err
}

func workspaceRecoveriesTx(tx ledger.Reader) ([]WorkspaceRecovery, error) {
	var raw string
	err := tx.QueryRow(`SELECT json_group_array(json_object('id',id,'state',state,'revision',revision,'data',data)) FROM operations WHERE kind=?`, workspaceRecoveryKind).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var envelopes []recoveryEnvelope
	if err := json.Unmarshal([]byte(raw), &envelopes); err != nil {
		return nil, err
	}
	out := make([]WorkspaceRecovery, 0, len(envelopes))
	for _, e := range envelopes {
		r, err := decodeWorkspaceRecovery(e)
		if err != nil {
			return nil, fmt.Errorf("workspace recovery %s: %w", e.ID, err)
		}
		if err := validateWorkspaceRecoveryTx(tx, r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func saveWorkspaceRecoveryTx(tx *ledger.Tx, r *WorkspaceRecovery, by string) error {
	var op ledger.Operation
	var raw string
	err := tx.QueryRow(`SELECT id,state,revision,data FROM operations WHERE kind=? AND id=?`, workspaceRecoveryKind, r.ID).Scan(&op.ID, &op.State, &op.Revision, &raw)
	if err != nil {
		return err
	}
	if op.Revision != r.Revision {
		return ledger.ErrConflict
	}
	op.Kind, op.Data = workspaceRecoveryKind, json.RawMessage(raw)
	r.Revision++
	op.Data, err = json.Marshal(r)
	if err != nil {
		return err
	}
	return tx.RecordTransition(op, r.Phase, by)
}

func (s *Service) startWorkspaceRecoveryTx(tx *ledger.Tx, r Record, base *RecoveryBaseline, at time.Time, owner string) (string, error) {
	if r.Workspace.Kind != project.KindCanonical {
		return "", nil
	}
	p, err := project.ReadTx(tx, r.Project)
	if err != nil {
		return "", err
	}
	if p.Home.Node != r.Workspace.Node || !samePhysicalPath(p.Home.Path, r.Workspace.Path) || p.Repo != project.RepoInPlace {
		return "", errors.New("original canonical declaration changed before recovery")
	}
	if base == nil || base.Name == "" || base.Version < 1 || base.Artifact == "" {
		return "", errors.New("canonical recovery requires a verified named artifact")
	}
	ref, found, err := tx.Name(base.Name)
	if err != nil {
		return "", err
	}
	if !found || ref.Version != base.Version || ref.Artifact != base.Artifact {
		return "", ledger.ErrConflict
	}
	all, err := workspaceRecoveriesTx(tx)
	if err != nil {
		return "", err
	}
	source := RecoverySource{Attempt: r.ID, Task: r.TaskID, Revision: r.ForceStop.Revision, At: at}
	declaration := project.RecoveryIdentity(p)
	for _, existing := range all {
		if existing.Target.Node != p.Home.Node || !samePhysicalPath(existing.Target.Path, p.Home.Path) {
			continue
		}
		if existing.Project != r.Project || existing.Declaration != declaration {
			return "", ErrWorkspaceRecovery
		}
		existing.Sources = append(existing.Sources, source)
		return existing.ID, saveWorkspaceRecoveryTx(tx, &existing, owner)
	}
	id := workspaceRecoverySourceID(r.ID, p.Home)
	next := WorkspaceRecovery{ID: id, Revision: 1, Phase: "recorded", CreatedAt: at, RequestedBy: owner, Project: r.Project, Declaration: declaration, Target: p.Home, Baseline: *base, Sources: []RecoverySource{source}, Head: RecoveryHead{Artifact: base.Artifact, ContentID: base.ContentID, Storage: base.Storage, Evidence: base.Evidence, Version: 1}}
	data, err := json.Marshal(next)
	if err != nil {
		return "", err
	}
	stamp := at.Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO operations(id,kind,state,revision,incarnation,data,created_at,updated_at) VALUES(?,?,?,1,?,?,?,?)`, id, workspaceRecoveryKind, next.Phase, s.l.Incarnation(), string(data), stamp, stamp); err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO events(operation_id,revision,incarnation,from_state,to_state,actor,fencings,effects,at) VALUES(?,1,?,'',?,?,'[]','null',?)`, id, s.l.Incarnation(), next.Phase, owner, stamp)
	return id, err
}
