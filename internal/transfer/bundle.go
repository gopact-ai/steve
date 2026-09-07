// Package transfer moves one complete project's durable facts in maintenance
// mode. It never transfers a hub database or a live ACP session.
package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

const Schema = 1

type Bundle struct {
	Schema     int                      `json:"schema"`
	Owner      project.Ownership        `json:"owner"`
	Project    project.ProjectTransfer  `json:"project"`
	Tasks      task.ProjectTransfer     `json:"tasks"`
	Plans      plan.ProjectTransfer     `json:"plans"`
	State      state.ProjectTransfer    `json:"state"`
	Schedules  schedule.ProjectTransfer `json:"schedules"`
	Console    console.ProjectTransfer  `json:"console"`
	Material   material.ProjectExport   `json:"material"`
	Artifacts  artifact.ProjectTransfer `json:"artifacts"`
	Facts      ledger.TransferFacts     `json:"facts"`
	Workspace  []byte                   `json:"workspace"`
	Snapshot   string                   `json:"snapshot"`
	GitHistory []byte                   `json:"git_history,omitempty"`
	GitCommit  string                   `json:"git_commit,omitempty"`
	GitBranch  string                   `json:"git_branch,omitempty"`
	NestedGit  map[string]NestedGit     `json:"nested_git,omitempty"`
	Memory     map[string][]byte        `json:"memory"`
	CreatedAt  time.Time                `json:"created_at"`
}
type Envelope struct {
	Digest string          `json:"digest"`
	Bundle json.RawMessage `json:"bundle"`
}
type Options struct{ StateDir, HubID, Project, TargetHub, TransferID, Evidence, Output, WorkspaceSnapshot string }

func releaseGuard(tx *ledger.Tx, id string) error {
	if err := attempt.ReleaseProjectGuard(tx, id); err != nil {
		return err
	}
	ops, err := tx.Operations("landing", "")
	if err != nil {
		return err
	}
	for _, op := range ops {
		var land artifact.Landing
		if err := json.Unmarshal(op.Data, &land); err != nil {
			return err
		}
		if land.Project == id && (op.State == artifact.LandApplying || op.State == artifact.LandRecoveryPending) {
			return fmt.Errorf("landing %s must be reconciled first", op.ID)
		}
	}
	clones, err := tx.Bindings("workspace-clone")
	if err != nil {
		return err
	}
	for _, raw := range clones {
		var c project.CloneOperation
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.Project == id && c.State != "succeeded" && c.State != "failed" {
			return fmt.Errorf("clone %s must be stopped before transfer", c.ID)
		}
	}
	return nil
}
func Export(ctx context.Context, o Options) (Bundle, error) {
	return exportProject(ctx, o, (*os.File).Sync)
}

var ErrClusterOfflineTransfer = errors.New("cluster-managed state cannot use offline project transfer; use coordinator handoff to move coordination between nodes in the same cluster")

func offlineLedger(ctx context.Context, book *ledger.Ledger) error {
	// A stopped replica still belongs to its cluster. An offline process has
	// no authority to release ownership or export only its old local files.
	err := book.Update(ctx, func(*ledger.Tx) error { return nil })
	if errors.Is(err, ledger.ErrReplicaUnavailable) {
		return fmt.Errorf("%w: %w", ErrClusterOfflineTransfer, err)
	}
	return err
}

func exportProject(ctx context.Context, o Options, syncFile func(*os.File) error) (Bundle, error) {
	var b Bundle
	if o.HubID == "" || o.Project == "" || o.TargetHub == "" || o.Output == "" || o.Evidence == "" {
		return b, errors.New("export requires hub, project, target, output and explicit stop evidence")
	}
	unlock, err := steveruntime.AcquireLock(o.StateDir)
	if err != nil {
		return b, err
	}
	defer unlock()
	book, err := ledger.Open(o.StateDir, ledger.Options{})
	if err != nil {
		return b, err
	}
	defer book.Close()
	if err := offlineLedger(ctx, book); err != nil {
		return b, err
	}
	projects := project.Open(book)
	projects.SetHubID(o.HubID)
	if err := book.Update(ctx, func(tx *ledger.Tx) error { return releaseGuard(tx, o.Project) }); err != nil {
		return b, err
	}
	b.Project, err = projects.ExportProject(ctx, o.Project)
	if err != nil {
		return b, err
	}
	p := b.Project.Project
	prior, exists, err := projects.Ownership(ctx, p.ID)
	if err != nil {
		return b, err
	}
	if exists && prior.State == "released" {
		return b, fmt.Errorf("project already released as %s; recover its original durable bundle instead of exporting new content", prior.TransferID)
	}
	source := o.WorkspaceSnapshot
	if source == "" {
		if p.Home.Node != "" {
			return b, errors.New("remote project requires --workspace-snapshot with its complete stopped offline directory")
		}
		source = p.Home.Path
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return b, err
	}
	b.Workspace, b.Snapshot, b.GitHistory, err = snapshot(ctx, source)
	if err != nil {
		return b, err
	}
	if len(b.GitHistory) > 0 {
		head, err := git(ctx, source, "rev-parse", "HEAD")
		if err != nil {
			return b, err
		}
		b.GitCommit = strings.TrimSpace(string(head))
		branch, _ := git(ctx, source, "symbolic-ref", "-q", "HEAD")
		b.GitBranch = strings.TrimSpace(string(branch))
	}
	b.NestedGit, err = captureNestedGit(ctx, source)
	if err != nil {
		return b, err
	}
	b.Tasks, err = task.ExportProject(book.Document("tasks"), p.ID)
	if err != nil {
		return b, err
	}
	chosen := map[string]bool{}
	conversations := map[string]bool{}
	for _, id := range b.Project.Conversations {
		conversations[id] = true
	}
	for id, t := range b.Tasks.Tasks {
		chosen[id] = true
		if t.Channel != "" {
			conversations[t.Channel] = true
		}
	}
	ids := make([]string, 0, len(conversations))
	for id := range conversations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	b.Plans, err = plan.ExportProject(book.Document("plans"), p.ID)
	if err != nil {
		return b, err
	}
	b.State, err = state.ExportProject(book.Document("state"), p.ID, ids)
	if err != nil {
		return b, err
	}
	b.Schedules, err = schedule.ExportProject(book.Document("schedules"), p.ID)
	if err != nil {
		return b, err
	}
	consoleIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.HasPrefix(id, "console:") {
			consoleIDs = append(consoleIDs, id)
		}
	}
	b.Console, err = console.ExportProject(book.Document("console"), p.ID, consoleIDs)
	if err != nil {
		return b, err
	}
	art := artifact.New(filepath.Join(o.StateDir, "artifacts"), book, projects, artifact.LocalNodes{Dir: o.StateDir})
	b.Artifacts, err = art.ExportProject(ctx, p.ID)
	if err != nil {
		return b, err
	}
	materials, err := material.Open(filepath.Join(o.StateDir, "materials"), book)
	if err != nil {
		return b, err
	}
	defer materials.Close()
	b.Material, err = materials.ExportProject(ctx, p.ID)
	if err != nil {
		return b, err
	}
	b.Facts.Add(b.Project.Facts)
	b.Facts.Add(b.Artifacts.Facts)
	facts, err := attempt.New(book).ExportProject(ctx, p.ID)
	if err != nil {
		return b, err
	}
	b.Facts.Add(facts)
	facts, err = exec.ExportProject(ctx, book, p.ID)
	if err != nil {
		return b, err
	}
	b.Facts.Add(facts)
	facts, err = intent.New(book).ExportTasks(ctx, chosen)
	if err != nil {
		return b, err
	}
	b.Facts.Add(facts)
	b.Memory, err = memory.ExportProjectFiles(filepath.Join(o.StateDir, "memory"), p.ID)
	if err != nil {
		return b, err
	}
	if o.TransferID == "" {
		bytes := make([]byte, 16)
		if _, err := rand.Read(bytes); err != nil {
			return b, err
		}
		o.TransferID = hex.EncodeToString(bytes)
	}
	old, found, err := projects.Ownership(ctx, p.ID)
	if err != nil {
		return b, err
	}
	if !found {
		old = project.Ownership{Project: p.ID, HubID: o.HubID, Epoch: 1, State: "active"}
	}
	b.Owner = old
	if old.State == "released" {
		if old.TransferID != o.TransferID || old.TargetHub != o.TargetHub {
			return b, errors.New("project already released in another transfer")
		}
	} else {
		if old.HubID != o.HubID {
			return b, project.ErrNotOwner
		}
		b.Owner.Epoch++
		b.Owner.State = "released"
		b.Owner.TargetHub = o.TargetHub
		b.Owner.TransferID = o.TransferID
		b.Owner.Evidence = o.Evidence
		b.Owner.UpdatedAt = time.Now().UTC()
	}
	b.Schema = Schema
	b.CreatedAt = time.Now().UTC()
	raw, err := json.Marshal(b)
	if err != nil {
		return b, err
	}
	wrapped, err := json.Marshal(Envelope{Digest: digest(raw), Bundle: raw})
	if err != nil {
		return b, err
	}
	if _, err := os.Lstat(o.Output); err == nil {
		return b, errors.New("output already exists; refusing to overwrite")
	}
	if err := os.MkdirAll(filepath.Dir(o.Output), 0700); err != nil {
		return b, err
	}
	temp := o.Output + ".partial-" + o.TransferID
	if err := (&ledger.FileDocument{Path: temp}).Save(wrapped); err != nil {
		return b, err
	}
	if err := syncTransferPath(temp, syncFile); err != nil {
		return b, fmt.Errorf("bundle is not durable; source remains owner: %w", err)
	}
	frozen := map[string]json.RawMessage{}
	for _, kind := range []string{"tasks", "schedules", "console"} {
		raw, exists, err := book.Document(kind).Load()
		if err != nil {
			return b, err
		}
		if !exists {
			continue
		}
		doc := &ledger.StagedDocument{Raw: raw, Exists: true}
		switch kind {
		case "tasks":
			err = task.FreezeProject(doc, p.ID)
		case "schedules":
			err = schedule.FreezeProject(doc, p.ID)
		case "console":
			err = console.FreezeProject(doc, p.ID)
		}
		if err != nil {
			return b, err
		}
		frozen[kind] = doc.Raw
	}
	guard := func(tx *ledger.Tx, id string) error {
		if err := releaseGuard(tx, id); err != nil {
			return err
		}
		for kind, raw := range frozen {
			if err := tx.StoreDocument(kind, raw); err != nil {
				return err
			}
		}
		return exec.FreezeProjectRunsTx(tx, id)
	}
	if _, err := projects.Release(ctx, p.ID, o.TargetHub, o.TransferID, o.Evidence, guard); err != nil {
		os.Remove(temp)
		return b, err
	}
	if err := os.Rename(temp, o.Output); err != nil {
		return b, fmt.Errorf("source released; recover bundle at %s: %w", temp, err)
	}
	if err := syncTransferPath(o.Output, syncFile); err != nil {
		return b, fmt.Errorf("source released; output rename durability uncertain at %s: %w", o.Output, err)
	}
	return b, nil
}
func Read(path string) (Bundle, error) {
	var b Bundle
	info, err := os.Stat(path)
	if err != nil {
		return b, err
	}
	if info.Size() > 2<<30 {
		return b, errors.New("bundle exceeds 2 GiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return b, err
	}
	if digest(e.Bundle) != e.Digest {
		return b, errors.New("bundle digest mismatch")
	}
	if err := json.Unmarshal(e.Bundle, &b); err != nil {
		return b, err
	}
	if b.Schema != Schema {
		return b, errors.New("unsupported migration schema")
	}
	return b, nil
}

type ImportOptions struct {
	TargetLevel                                  project.Level
	StateDir, HubID, ExpectedSource, Home, Input string
	Finalize                                     func(project.Project) error
}

func Import(ctx context.Context, o ImportOptions) (project.Project, error) {
	return importProject(ctx, o, (*os.File).Sync)
}

func importProject(ctx context.Context, o ImportOptions, syncFile func(*os.File) error) (project.Project, error) {
	var empty project.Project
	b, err := Read(o.Input)
	if err != nil {
		return empty, err
	}
	originalBytes, _ := json.Marshal(b)
	bundleIdentity := digest(originalBytes)
	if err := validateBundle(b); err != nil {
		return empty, err
	}
	if !b.Project.Project.Level.OrDefault().Admits(o.TargetLevel.OrDefault()) {
		return empty, errors.New("target hub data level cannot hold this project")
	}
	if o.Home == "" {
		return empty, errors.New("import requires a new local home")
	}
	o.Home, err = canonicalTransferPath(o.Home)
	if err != nil {
		return empty, err
	}
	if err := remapBundle(&b, o.Home); err != nil {
		return empty, err
	}
	p := b.Project.Project
	if o.HubID == "" || o.HubID != b.Owner.TargetHub || o.ExpectedSource == "" || o.ExpectedSource != b.Owner.HubID || o.Home == "" {
		return empty, errors.New("import requires matching target/source identities and a new local home")
	}
	if p.ID == "" || strings.ContainsAny(p.ID, "/\\") || b.Owner.Project != p.ID {
		return empty, errors.New("invalid project identity")
	}
	for _, id := range []string{b.Tasks.Project, b.Plans.Project, b.State.Project, b.Schedules.Project, b.Console.Project, b.Material.Project, b.Artifacts.Project} {
		if id != p.ID {
			return empty, errors.New("cross-project bundle component")
		}
	}
	unlock, err := steveruntime.AcquireLock(o.StateDir)
	if err != nil {
		return empty, err
	}
	defer unlock()
	book, err := ledger.Open(o.StateDir, ledger.Options{})
	if err != nil {
		return empty, err
	}
	defer book.Close()
	if err := offlineLedger(ctx, book); err != nil {
		return empty, err
	}
	projects := project.Open(book)
	projects.SetHubID(o.HubID)
	existing, ok, err := projects.Ownership(ctx, p.ID)
	if err != nil {
		return empty, err
	}
	if ok {
		if existing.HubID == o.HubID && (existing.State == "active" || existing.State == "importing") && existing.Epoch == b.Owner.Epoch && existing.TransferID == b.Owner.TransferID {
			var receipt struct{ Digest, Home string }
			if found, err := book.GetBinding(ctx, "project-transfer-receipt", b.Owner.TransferID, &receipt); err != nil || !found || receipt.Digest != bundleIdentity {
				return empty, errors.New("same-transfer replay has different or missing content receipt")
			}
			wanted, _ := filepath.Abs(o.Home)
			if receipt.Home != wanted {
				return empty, errors.New("same transfer cannot move its accepted home")
			}
			restored, _, err := projects.Lookup(ctx, p.ID)
			if err == nil && o.Finalize != nil {
				err = o.Finalize(restored)
			}
			if err == nil {
				err = projects.ActivateTransfer(ctx, p.ID, b.Owner.TransferID, b.Owner.Epoch)
			}
			return restored, err
		}
		return empty, errors.New("ownership collision or stale transfer")
	}
	if _, exists, err := projects.Lookup(ctx, p.ID); err != nil || exists {
		if err == nil {
			err = errors.New("target project already exists")
		}
		return empty, err
	}
	staged := map[string]*ledger.StagedDocument{}
	expected := map[string]json.RawMessage{}
	for _, kind := range []string{"tasks", "plans", "state", "schedules", "console"} {
		raw, exists, err := book.Document(kind).Load()
		if err != nil {
			return empty, err
		}
		expected[kind] = raw
		staged[kind] = &ledger.StagedDocument{Raw: raw, Exists: exists}
	}
	for _, merge := range []func() error{func() error { return task.ImportProject(staged["tasks"], b.Tasks) }, func() error { return plan.ImportProject(staged["plans"], b.Plans) }, func() error { return state.ImportProject(staged["state"], b.State) }, func() error { return schedule.ImportProject(staged["schedules"], b.Schedules) }, func() error { return console.ImportProject(staged["console"], b.Console) }} {
		if err := merge(); err != nil {
			return empty, err
		}
	}
	stageDir, err := os.MkdirTemp(o.StateDir, ".transfer-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(stageDir)
	stageBook, err := ledger.Open(filepath.Join(stageDir, "ledger"), ledger.Options{})
	if err != nil {
		return empty, err
	}
	defer stageBook.Close()
	materials, err := material.Open(filepath.Join(stageDir, "materials"), stageBook)
	if err != nil {
		return empty, err
	}
	defer materials.Close()
	if err := materials.ImportProject(ctx, b.Material); err != nil {
		return empty, err
	}
	targetHome, err := filepath.Abs(o.Home)
	if err != nil {
		return empty, err
	}
	p.Home = project.Home{Path: targetHome}
	p.Copies = nil
	p.DurablePlaces = nil
	stageProjects := project.Open(stageBook)
	stageProjects.SetHubID(o.HubID)
	targetProjects, err := book.Bindings(ctx, "project")
	if err != nil {
		return empty, err
	}
	for id, raw := range targetProjects {
		var occupied project.Project
		if err := json.Unmarshal(raw, &occupied); err != nil {
			return empty, err
		}
		if occupied.Home.Node == "" {
			occupied.Home.Path, err = canonicalTransferPath(occupied.Home.Path)
			if err != nil {
				return empty, err
			}
		}
		for node, copy := range occupied.Copies {
			if copy.Node == "" {
				copy.Path, err = canonicalTransferPath(copy.Path)
				if err != nil {
					return empty, err
				}
				occupied.Copies[node] = copy
			}
		}
		raw, _ = json.Marshal(occupied)
		if err := stageBook.PutBinding(ctx, "project", id, raw); err != nil {
			return empty, err
		}
	}
	if err := stageProjects.Accept(ctx, p, b.Owner); err != nil {
		return empty, err
	}
	pendingOwner, _, err := stageProjects.Ownership(ctx, p.ID)
	if err != nil {
		return empty, err
	}
	pendingOwner.State = "importing"
	if err := stageBook.PutBinding(ctx, "project-owner", p.ID, pendingOwner); err != nil {
		return empty, err
	}
	facts := b.Facts
	receiptRaw, _ := json.Marshal(struct{ Digest, Home string }{bundleIdentity, targetHome})
	facts.Add(ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{"project-transfer-receipt": {b.Owner.TransferID: receiptRaw}}})
	for _, kind := range []string{"project", "project-owner"} {
		raw, err := stageBook.Bindings(ctx, kind)
		if err != nil {
			return empty, err
		}
		facts.Add(ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{kind: {p.ID: raw[p.ID]}}})
	}
	for _, kind := range []string{"material", "material-annotation"} {
		raw, err := stageBook.Bindings(ctx, kind)
		if err != nil {
			return empty, err
		}
		facts.Add(ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{kind: raw}})
	}
	docs := map[string]json.RawMessage{}
	for kind, doc := range staged {
		docs[kind] = doc.Raw
	}
	if err := book.ValidateImport(ctx, facts, docs, expected); err != nil {
		return empty, err
	}
	targetHome, err = filepath.Abs(o.Home)
	if err != nil {
		return empty, err
	}
	inputBytes, err := os.ReadFile(o.Input)
	if err != nil {
		return empty, err
	}
	if strings.ContainsAny(b.Owner.TransferID, "/\\") || b.Owner.TransferID == "" {
		return empty, errors.New("invalid transfer id")
	}
	marker := filepath.Join(o.StateDir, "transfers", b.Owner.TransferID+".json")
	install := struct{ Digest, Home, Project string }{digest(inputBytes), targetHome, p.ID}
	markerBytes, _ := json.Marshal(install)
	retry := false
	if previous, err := os.ReadFile(marker); err == nil {
		if string(previous) != string(markerBytes) {
			return empty, errors.New("transfer install marker mismatch")
		}
		retry = true
	} else if !os.IsNotExist(err) {
		return empty, err
	}
	if !retry {
		if entries, err := os.ReadDir(targetHome); err == nil && len(entries) > 0 {
			return empty, errors.New("target home must be empty")
		} else if err != nil && !os.IsNotExist(err) {
			return empty, err
		}
		if _, err := os.Stat(filepath.Join(o.StateDir, "artifacts", "objects", p.ID+".git")); err == nil {
			return empty, errors.New("target artifact repository already exists")
		}
		for name := range b.Memory {
			if _, err := os.Stat(filepath.Join(o.StateDir, "memory", "projects", name)); err == nil {
				return empty, errors.New("target memory already exists")
			}
		}
		if err := (&ledger.FileDocument{Path: marker}).Save(markerBytes); err != nil {
			return empty, err
		}
	}
	if err := restoreSnapshot(ctx, b.Workspace, b.Snapshot, targetHome); err != nil {
		return empty, err
	}
	if err := restoreGitHistory(ctx, b.GitHistory, b.GitCommit, b.GitBranch, targetHome, stageDir); err != nil {
		return empty, err
	}
	if err := restoreNestedGit(ctx, b.NestedGit, targetHome, stageDir); err != nil {
		return empty, err
	}
	if err := artifact.ImportProjectObjects(ctx, filepath.Join(o.StateDir, "artifacts"), b.Artifacts, retry); err != nil {
		return empty, err
	}
	if len(b.GitHistory) > 0 {
		history := filepath.Join(o.StateDir, "artifacts", "source-history", p.ID+".bundle")
		if err := (&ledger.FileDocument{Path: history}).Save(b.GitHistory); err != nil {
			return empty, err
		}
	}
	actualMaterials, err := material.Open(filepath.Join(o.StateDir, "materials"), book)
	if err != nil {
		return empty, err
	}
	defer actualMaterials.Close()
	// Blob files are content addressed and inert until their metadata commits.
	if err := actualMaterials.StageBlobs(b.Material); err != nil {
		return empty, err
	}
	for name, raw := range b.Memory {
		if name != p.ID+".md" && name != p.ID+".md.requests.jsonl" && name != p.ID+".md.pending.json" && name != p.ID+".audit.jsonl" {
			return empty, errors.New("invalid memory path")
		}
		path := filepath.Join(o.StateDir, "memory", "projects", name)
		if old, err := os.ReadFile(path); err == nil {
			if string(old) != string(raw) {
				return empty, errors.New("target project memory changed")
			}
			continue
		} else if !os.IsNotExist(err) {
			return empty, err
		}
		if err := (&ledger.FileDocument{Path: path}).Save(raw); err != nil {
			return empty, err
		}
	}
	// Ownership must not become durable before the files it authorizes. Git
	// and copied workspace files do not themselves guarantee a durable tree.
	paths := []string{targetHome, marker}
	if len(b.Artifacts.GitBundle) > 0 {
		paths = append(paths, filepath.Join(o.StateDir, "artifacts", "objects", p.ID+".git"))
	}
	if len(b.GitHistory) > 0 {
		paths = append(paths, filepath.Join(o.StateDir, "artifacts", "source-history", p.ID+".bundle"))
	}
	for id := range b.Material.Blobs {
		paths = append(paths, filepath.Join(o.StateDir, "materials", id))
	}
	for name := range b.Memory {
		paths = append(paths, filepath.Join(o.StateDir, "memory", "projects", name))
	}
	for _, path := range paths {
		if err := syncTransferPath(path, syncFile); err != nil {
			return empty, fmt.Errorf("migration files not durable; project remains inactive: %w", err)
		}
	}
	if err := book.ImportFacts(ctx, facts, docs, expected); err != nil {
		return empty, err
	}
	p.Home = project.Home{Path: targetHome}
	if o.Finalize != nil {
		if err := o.Finalize(p); err != nil {
			return p, fmt.Errorf("project data imported but inactive; rerun import to finish configuration: %w", err)
		}
	}
	if err := projects.ActivateTransfer(ctx, p.ID, b.Owner.TransferID, b.Owner.Epoch); err != nil {
		return p, err
	}
	return p, nil
}
