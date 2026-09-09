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
	"log/slog"
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
		if c.Project == id && c.State != project.CloneSucceeded && c.State != project.CloneFailed {
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

// exportProject moves a stopped project out of this hub in the order the
// durability rules demand: read everything, seal it into a durable bundle,
// release ownership, then give the bundle its final name. Each stage is a
// function below; the order of their errors is the order of the stages.
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
	if err := refuseReleasedProject(ctx, projects, p.ID); err != nil {
		return b, err
	}
	if err := exportWorkspace(ctx, o, p, &b); err != nil {
		return b, err
	}
	if err := exportDocuments(book, p.ID, &b); err != nil {
		return b, err
	}
	if err := exportContent(ctx, o, book, projects, p.ID, &b); err != nil {
		return b, err
	}
	if o.TransferID == "" {
		if o.TransferID, err = newTransferID(); err != nil {
			return b, err
		}
	}
	b.Owner, err = releasedOwner(ctx, projects, o, p.ID)
	if err != nil {
		return b, err
	}
	b.Schema = Schema
	b.CreatedAt = time.Now().UTC()
	temp, err := writeBundle(o, b, syncFile)
	if err != nil {
		return b, err
	}
	frozen, err := frozenDocuments(book, p.ID)
	if err != nil {
		return b, err
	}
	if err := releaseSource(ctx, projects, o, p.ID, frozen); err != nil {
		// The source still owns the project, so the partial bundle has no
		// reader; one that resists removal is only worth a warning.
		if removeErr := os.Remove(temp); removeErr != nil {
			slog.Warn(fmt.Sprintf("transfer: partial bundle %s left behind: %v", temp, removeErr), "project", p.ID, "transfer", o.TransferID)
		}
		return b, err
	}
	return b, publishBundle(temp, o.Output, syncFile)
}

// refuseReleasedProject stops a second export of a project this hub has
// already handed over: the bundle written then is the one to recover.
func refuseReleasedProject(ctx context.Context, projects *project.Store, id string) error {
	prior, exists, err := projects.Ownership(ctx, id)
	if err != nil {
		return err
	}
	if exists && prior.State == "released" {
		return fmt.Errorf("project already released as %s; recover its original durable bundle instead of exporting new content", prior.TransferID)
	}
	return nil
}

// exportWorkspace snapshots the stopped working tree into the bundle: its
// files, the repository history and head when the tree is a repository,
// and every nested repository.
func exportWorkspace(ctx context.Context, o Options, p project.Project, b *Bundle) error {
	source := o.WorkspaceSnapshot
	if source == "" {
		if p.Home.Node != "" {
			return errors.New("remote project requires --workspace-snapshot with its complete stopped offline directory")
		}
		source = p.Home.Path
	}
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	b.Workspace, b.Snapshot, b.GitHistory, err = snapshot(ctx, source)
	if err != nil {
		return err
	}
	if len(b.GitHistory) > 0 {
		b.GitCommit, b.GitBranch, err = gitHead(ctx, source)
		if err != nil {
			return err
		}
	}
	b.NestedGit, err = captureNestedGit(ctx, source)
	return err
}

// gitHead is the commit HEAD names in dir and, unless HEAD is detached,
// the branch it is on.
func gitHead(ctx context.Context, dir string) (commit, branch string, err error) {
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	// symbolic-ref fails on a detached HEAD, and an empty branch is how the
	// bundle records one; rev-parse just proved the repository is readable.
	ref, _ := git(ctx, dir, "symbolic-ref", "-q", "HEAD")
	return strings.TrimSpace(string(head)), strings.TrimSpace(string(ref)), nil
}

// exportDocuments copies the project's rows out of each shared document.
// Tasks go first: their channels decide which conversations come along.
func exportDocuments(book *ledger.Ledger, id string, b *Bundle) error {
	var err error
	b.Tasks, err = task.ExportProject(book.Document("tasks"), id)
	if err != nil {
		return err
	}
	conversations, consoleIDs := transferConversations(b)
	b.Plans, err = plan.ExportProject(book.Document("plans"), id)
	if err != nil {
		return err
	}
	b.State, err = state.ExportProject(book.Document("state"), id, conversations)
	if err != nil {
		return err
	}
	b.Schedules, err = schedule.ExportProject(book.Document("schedules"), id)
	if err != nil {
		return err
	}
	b.Console, err = console.ExportProject(book.Document("console"), id, consoleIDs)
	return err
}

// transferConversations lists, in a stable order, the conversations the
// bundle carries — the project's own and every task channel — and, of
// those, the console ones.
func transferConversations(b *Bundle) (all, console []string) {
	seen := map[string]bool{}
	for _, id := range b.Project.Conversations {
		seen[id] = true
	}
	for _, t := range b.Tasks.Tasks {
		if t.Channel != "" {
			seen[t.Channel] = true
		}
	}
	all = make([]string, 0, len(seen))
	for id := range seen {
		all = append(all, id)
	}
	sort.Strings(all)
	console = make([]string, 0, len(all))
	for _, id := range all {
		if strings.HasPrefix(id, "console:") {
			console = append(console, id)
		}
	}
	return all, console
}

// exportContent gathers what lives outside the shared documents: the
// artifact repository, the material store, the ledger facts of attempts,
// runs and intents, and the project's memory files.
func exportContent(ctx context.Context, o Options, book *ledger.Ledger, projects *project.Store, id string, b *Bundle) error {
	art := artifact.New(filepath.Join(o.StateDir, "artifacts"), book, projects, artifact.LocalNodes{Dir: o.StateDir})
	var err error
	b.Artifacts, err = art.ExportProject(ctx, id)
	if err != nil {
		return err
	}
	materials, err := material.Open(filepath.Join(o.StateDir, "materials"), book)
	if err != nil {
		return err
	}
	defer materials.Close()
	b.Material, err = materials.ExportProject(ctx, id)
	if err != nil {
		return err
	}
	b.Facts.Add(b.Project.Facts)
	b.Facts.Add(b.Artifacts.Facts)
	facts, err := attempt.New(book).ExportProject(ctx, id)
	if err != nil {
		return err
	}
	b.Facts.Add(facts)
	facts, err = exec.ExportProject(ctx, book, id)
	if err != nil {
		return err
	}
	b.Facts.Add(facts)
	chosen := map[string]bool{}
	for taskID := range b.Tasks.Tasks {
		chosen[taskID] = true
	}
	facts, err = intent.New(book).ExportTasks(ctx, chosen)
	if err != nil {
		return err
	}
	b.Facts.Add(facts)
	b.Memory, err = memory.ExportProjectFiles(filepath.Join(o.StateDir, "memory"), id)
	return err
}

// newTransferID names a transfer the caller did not name.
func newTransferID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// releasedOwner is the ownership record the bundle carries: the current
// record advanced to "released" toward the target, or the release already
// recorded for this same transfer. A record this hub does not own, or a
// release toward someone else, refuses the export.
func releasedOwner(ctx context.Context, projects *project.Store, o Options, id string) (project.Ownership, error) {
	owner, found, err := projects.Ownership(ctx, id)
	if err != nil {
		return owner, err
	}
	if !found {
		owner = project.Ownership{Project: id, HubID: o.HubID, Epoch: 1, State: "active"}
	}
	if owner.State == "released" {
		if owner.TransferID != o.TransferID || owner.TargetHub != o.TargetHub {
			return owner, errors.New("project already released in another transfer")
		}
		return owner, nil
	}
	if owner.HubID != o.HubID {
		return owner, project.ErrNotOwner
	}
	owner.Epoch++
	owner.State = "released"
	owner.TargetHub = o.TargetHub
	owner.TransferID = o.TransferID
	owner.Evidence = o.Evidence
	owner.UpdatedAt = time.Now().UTC()
	return owner, nil
}

// writeBundle seals the bundle under a digest and writes it durably beside
// the output under a partial name, so the release never refers to a bundle
// that could still vanish. It returns the partial path.
func writeBundle(o Options, b Bundle, syncFile func(*os.File) error) (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	wrapped, err := json.Marshal(Envelope{Digest: digest(raw), Bundle: raw})
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(o.Output); err == nil {
		return "", errors.New("output already exists; refusing to overwrite")
	}
	if err := os.MkdirAll(filepath.Dir(o.Output), 0700); err != nil {
		return "", err
	}
	temp := o.Output + ".partial-" + o.TransferID
	if err := (&ledger.FileDocument{Path: temp}).Save(wrapped); err != nil {
		return "", err
	}
	if err := syncTransferPath(temp, syncFile); err != nil {
		return "", fmt.Errorf("bundle is not durable; source remains owner: %w", err)
	}
	return temp, nil
}

// frozenDocuments is each shared document that carries per-project rows,
// with this project's rows frozen, ready to store in the release.
func frozenDocuments(book *ledger.Ledger, id string) (map[string]json.RawMessage, error) {
	frozen := map[string]json.RawMessage{}
	for _, kind := range []string{"tasks", "schedules", "console"} {
		raw, exists, err := book.Document(kind).Load()
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		doc := &ledger.StagedDocument{Raw: raw, Exists: true}
		switch kind {
		case "tasks":
			err = task.FreezeProject(doc, id)
		case "schedules":
			err = schedule.FreezeProject(doc, id)
		case "console":
			err = console.FreezeProject(doc, id)
		}
		if err != nil {
			return nil, err
		}
		frozen[kind] = doc.Raw
	}
	return frozen, nil
}

// releaseSource hands the project over in one transaction: the guard is
// checked again under the lock, the frozen documents and runs are stored,
// and ownership moves to the target.
func releaseSource(ctx context.Context, projects *project.Store, o Options, id string, frozen map[string]json.RawMessage) error {
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
	_, err := projects.Release(ctx, id, o.TargetHub, o.TransferID, o.Evidence, guard)
	return err
}

// publishBundle gives the durable bundle its final name once the source
// has released. From here on the file is the only copy of the project, so
// every error names where it is.
func publishBundle(temp, output string, syncFile func(*os.File) error) error {
	if err := os.Rename(temp, output); err != nil {
		return fmt.Errorf("source released; recover bundle at %s: %w", temp, err)
	}
	if err := syncTransferPath(output, syncFile); err != nil {
		return fmt.Errorf("source released; output rename durability uncertain at %s: %w", output, err)
	}
	return nil
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

// importProject brings a released project into this hub: it admits the
// bundle, stages every ledger change beside the live ledger and validates
// it, claims the places on disk, installs the files durably, and only then
// commits the facts and activates ownership. An import interrupted after
// the commit is finished by resumeAcceptedImport on the next run; one
// interrupted before it is retried against the install marker.
func importProject(ctx context.Context, o ImportOptions, syncFile func(*os.File) error) (project.Project, error) {
	var empty project.Project
	b, identity, err := readImportBundle(o.Input)
	if err != nil {
		return empty, err
	}
	if err := admitImport(b, o); err != nil {
		return empty, err
	}
	o.Home, err = canonicalTransferPath(o.Home)
	if err != nil {
		return empty, err
	}
	if err := remapBundle(&b, o.Home); err != nil {
		return empty, err
	}
	if err := checkImportIdentities(b, o); err != nil {
		return empty, err
	}
	p := b.Project.Project
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
		if !sameTransfer(existing, b.Owner, o.HubID) {
			return empty, errors.New("ownership collision or stale transfer")
		}
		return resumeAcceptedImport(ctx, book, projects, o, b, identity)
	}
	if _, exists, err := projects.Lookup(ctx, p.ID); err != nil || exists {
		if err == nil {
			err = errors.New("target project already exists")
		}
		return empty, err
	}
	staged, expected, err := stageDocuments(book, b)
	if err != nil {
		return empty, err
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
	if err := stageMaterials(ctx, stageDir, stageBook, b.Material); err != nil {
		return empty, err
	}
	targetHome, err := filepath.Abs(o.Home)
	if err != nil {
		return empty, err
	}
	p.Home = project.Home{Path: targetHome}
	p.Copies = nil
	p.DurablePlaces = nil
	if err := stageOwnership(ctx, book, stageBook, o.HubID, p, b.Owner); err != nil {
		return empty, err
	}
	run := importRun{o: o, b: b, id: p.ID, identity: identity, targetHome: targetHome, stageDir: stageDir}
	facts, err := run.importedFacts(ctx, stageBook)
	if err != nil {
		return empty, err
	}
	docs := map[string]json.RawMessage{}
	for kind, doc := range staged {
		docs[kind] = doc.Raw
	}
	if err := book.ValidateImport(ctx, facts, docs, expected); err != nil {
		return empty, err
	}
	marker, retry, err := run.claimInstall()
	if err != nil {
		return empty, err
	}
	if err := run.installFiles(ctx, book, retry); err != nil {
		return empty, err
	}
	if err := run.installMemory(); err != nil {
		return empty, err
	}
	// Ownership must not become durable before the files it authorizes. Git
	// and copied workspace files do not themselves guarantee a durable tree.
	for _, path := range run.installedPaths(marker) {
		if err := syncTransferPath(path, syncFile); err != nil {
			return empty, fmt.Errorf("migration files not durable; project remains inactive: %w", err)
		}
	}
	if err := book.ImportFacts(ctx, facts, docs, expected); err != nil {
		return empty, err
	}
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

// readImportBundle reads the bundle and names its content: the digest of
// the bundle as decoded, which the receipt of an accepted import repeats.
func readImportBundle(path string) (Bundle, string, error) {
	b, err := Read(path)
	if err != nil {
		return b, "", err
	}
	original, err := json.Marshal(b)
	if err != nil {
		return b, "", err
	}
	return b, digest(original), nil
}

// admitImport checks what can be known before the ledger opens: the bundle
// is internally consistent, this hub's data level can hold the project,
// and the caller named a home for it.
func admitImport(b Bundle, o ImportOptions) error {
	if err := validateBundle(b); err != nil {
		return err
	}
	if !b.Project.Project.Level.OrDefault().Admits(o.TargetLevel.OrDefault()) {
		return errors.New("target hub data level cannot hold this project")
	}
	if o.Home == "" {
		return errors.New("import requires a new local home")
	}
	return nil
}

// checkImportIdentities confirms the bundle was released to this hub by
// the hub the caller expects, and that every component names one project.
func checkImportIdentities(b Bundle, o ImportOptions) error {
	p := b.Project.Project
	if o.HubID == "" || o.HubID != b.Owner.TargetHub || o.ExpectedSource == "" || o.ExpectedSource != b.Owner.HubID || o.Home == "" {
		return errors.New("import requires matching target/source identities and a new local home")
	}
	if p.ID == "" || strings.ContainsAny(p.ID, "/\\") || b.Owner.Project != p.ID {
		return errors.New("invalid project identity")
	}
	for _, id := range []string{b.Tasks.Project, b.Plans.Project, b.State.Project, b.Schedules.Project, b.Console.Project, b.Material.Project, b.Artifacts.Project} {
		if id != p.ID {
			return errors.New("cross-project bundle component")
		}
	}
	return nil
}

// sameTransfer reports whether the ownership this hub already holds came
// from the very transfer the bundle describes, still awaiting activation
// or already active.
func sameTransfer(existing, released project.Ownership, hubID string) bool {
	return existing.HubID == hubID && (existing.State == "active" || existing.State == "importing") && existing.Epoch == released.Epoch && existing.TransferID == released.TransferID
}

// resumeAcceptedImport finishes an import whose facts already committed:
// the receipt must describe this bundle and this home, and only
// configuration and activation are left to do.
func resumeAcceptedImport(ctx context.Context, book *ledger.Ledger, projects *project.Store, o ImportOptions, b Bundle, identity string) (project.Project, error) {
	var empty project.Project
	var receipt struct{ Digest, Home string }
	if found, err := book.GetBinding(ctx, "project-transfer-receipt", b.Owner.TransferID, &receipt); err != nil || !found || receipt.Digest != identity {
		return empty, errors.New("same-transfer replay has different or missing content receipt")
	}
	wanted, err := filepath.Abs(o.Home)
	if err != nil {
		return empty, err
	}
	if receipt.Home != wanted {
		return empty, errors.New("same transfer cannot move its accepted home")
	}
	restored, _, err := projects.Lookup(ctx, b.Project.Project.ID)
	if err == nil && o.Finalize != nil {
		err = o.Finalize(restored)
	}
	if err == nil {
		err = projects.ActivateTransfer(ctx, b.Project.Project.ID, b.Owner.TransferID, b.Owner.Epoch)
	}
	return restored, err
}

// stageDocuments merges the bundle's rows into a staged copy of each shared
// document; expected keeps the originals the commit must still find.
func stageDocuments(book *ledger.Ledger, b Bundle) (map[string]*ledger.StagedDocument, map[string]json.RawMessage, error) {
	staged := map[string]*ledger.StagedDocument{}
	expected := map[string]json.RawMessage{}
	for _, kind := range []string{"tasks", "plans", "state", "schedules", "console"} {
		raw, exists, err := book.Document(kind).Load()
		if err != nil {
			return nil, nil, err
		}
		expected[kind] = raw
		staged[kind] = &ledger.StagedDocument{Raw: raw, Exists: exists}
	}
	for _, merge := range []func() error{func() error { return task.ImportProject(staged["tasks"], b.Tasks) }, func() error { return plan.ImportProject(staged["plans"], b.Plans) }, func() error { return state.ImportProject(staged["state"], b.State) }, func() error { return schedule.ImportProject(staged["schedules"], b.Schedules) }, func() error { return console.ImportProject(staged["console"], b.Console) }} {
		if err := merge(); err != nil {
			return nil, nil, err
		}
	}
	return staged, expected, nil
}

// stageMaterials imports the material metadata into the staging ledger,
// where importedFacts reads it back as bindings.
func stageMaterials(ctx context.Context, stageDir string, stageBook *ledger.Ledger, in material.ProjectExport) error {
	materials, err := material.Open(filepath.Join(stageDir, "materials"), stageBook)
	if err != nil {
		return err
	}
	defer materials.Close()
	return materials.ImportProject(ctx, in)
}

// stageOwnership accepts the project into the staging ledger as the
// target hub would, with every other project's home beside it so the
// acceptance refuses an aliased directory, and leaves ownership
// "importing" until the files are durable.
func stageOwnership(ctx context.Context, book, stageBook *ledger.Ledger, hubID string, p project.Project, owner project.Ownership) error {
	stageProjects := project.Open(stageBook)
	stageProjects.SetHubID(hubID)
	if err := stageOccupiedHomes(ctx, book, stageBook); err != nil {
		return err
	}
	if err := stageProjects.Accept(ctx, p, owner); err != nil {
		return err
	}
	pendingOwner, _, err := stageProjects.Ownership(ctx, p.ID)
	if err != nil {
		return err
	}
	pendingOwner.State = "importing"
	return stageBook.PutBinding(ctx, "project-owner", p.ID, pendingOwner)
}

// stageOccupiedHomes copies the live ledger's projects into the staging
// ledger with their local paths resolved, since directory ownership must
// compare physical paths.
func stageOccupiedHomes(ctx context.Context, book, stageBook *ledger.Ledger) error {
	targetProjects, err := book.Bindings(ctx, "project")
	if err != nil {
		return err
	}
	for id, raw := range targetProjects {
		var occupied project.Project
		if err := json.Unmarshal(raw, &occupied); err != nil {
			return err
		}
		if occupied.Home.Node == "" {
			occupied.Home.Path, err = canonicalTransferPath(occupied.Home.Path)
			if err != nil {
				return err
			}
		}
		for node, copy := range occupied.Copies {
			if copy.Node == "" {
				copy.Path, err = canonicalTransferPath(copy.Path)
				if err != nil {
					return err
				}
				occupied.Copies[node] = copy
			}
		}
		if raw, err = json.Marshal(occupied); err != nil {
			return err
		}
		if err := stageBook.PutBinding(ctx, "project", id, raw); err != nil {
			return err
		}
	}
	return nil
}

// importRun is what every stage below the staging ledger shares: the
// options as given, the bundle being installed, and the identity, project
// and places derived from them once. The stages read it instead of
// repeating the same interchangeable strings positionally.
type importRun struct {
	o        ImportOptions
	b        Bundle
	id       string
	identity string
	// targetHome is the absolute home the project takes here; stageDir is
	// the temporary directory the install rebuilds repositories in.
	targetHome, stageDir string
}

// importedFacts is everything the commit writes to the live ledger: the
// bundle's facts, the receipt that lets the same transfer resume, and the
// project, ownership and material bindings as staged.
func (r importRun) importedFacts(ctx context.Context, stageBook *ledger.Ledger) (ledger.TransferFacts, error) {
	facts := r.b.Facts
	receiptRaw, err := json.Marshal(struct{ Digest, Home string }{r.identity, r.targetHome})
	if err != nil {
		return facts, err
	}
	facts.Add(ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{"project-transfer-receipt": {r.b.Owner.TransferID: receiptRaw}}})
	for _, kind := range []string{"project", "project-owner"} {
		raw, err := stageBook.Bindings(ctx, kind)
		if err != nil {
			return facts, err
		}
		facts.Add(ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{kind: {r.id: raw[r.id]}}})
	}
	for _, kind := range []string{"material", "material-annotation"} {
		raw, err := stageBook.Bindings(ctx, kind)
		if err != nil {
			return facts, err
		}
		facts.Add(ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{kind: raw}})
	}
	return facts, nil
}

// claimInstall marks the target home, artifact repository and memory as
// this transfer's before anything is written there. A marker left by an
// interrupted import must describe the same payload and places; the
// install is then a retry that tolerates what is already in place.
func (r importRun) claimInstall() (marker string, retry bool, err error) {
	inputBytes, err := os.ReadFile(r.o.Input)
	if err != nil {
		return "", false, err
	}
	if strings.ContainsAny(r.b.Owner.TransferID, "/\\") || r.b.Owner.TransferID == "" {
		return "", false, errors.New("invalid transfer id")
	}
	marker = filepath.Join(r.o.StateDir, "transfers", r.b.Owner.TransferID+".json")
	markerBytes, err := json.Marshal(struct{ Digest, Home, Project string }{digest(inputBytes), r.targetHome, r.id})
	if err != nil {
		return "", false, err
	}
	if previous, err := os.ReadFile(marker); err == nil {
		if string(previous) != string(markerBytes) {
			return "", false, errors.New("transfer install marker mismatch")
		}
		return marker, true, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	if err := r.refuseOccupiedTargets(); err != nil {
		return "", false, err
	}
	if err := (&ledger.FileDocument{Path: marker}).Save(markerBytes); err != nil {
		return "", false, err
	}
	return marker, false, nil
}

// refuseOccupiedTargets stops a fresh install from writing where another
// project, or an earlier life of this one, already keeps files.
func (r importRun) refuseOccupiedTargets() error {
	if entries, err := os.ReadDir(r.targetHome); err == nil && len(entries) > 0 {
		return errors.New("target home must be empty")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.o.StateDir, "artifacts", "objects", r.id+".git")); err == nil {
		return errors.New("target artifact repository already exists")
	}
	for name := range r.b.Memory {
		if _, err := os.Stat(filepath.Join(r.o.StateDir, "memory", "projects", name)); err == nil {
			return errors.New("target memory already exists")
		}
	}
	return nil
}

// installFiles restores the working tree, its history and nested
// repositories, the artifact repository, the source history and the
// material blobs. Each step accepts its own earlier, identical result so
// an interrupted install can be retried.
func (r importRun) installFiles(ctx context.Context, book *ledger.Ledger, retry bool) error {
	if err := restoreSnapshot(ctx, r.b.Workspace, r.b.Snapshot, r.targetHome); err != nil {
		return err
	}
	if err := restoreGitHistory(ctx, r.b.GitHistory, r.b.GitCommit, r.b.GitBranch, r.targetHome, r.stageDir); err != nil {
		return err
	}
	if err := restoreNestedGit(ctx, r.b.NestedGit, r.targetHome, r.stageDir); err != nil {
		return err
	}
	if err := artifact.ImportProjectObjects(ctx, filepath.Join(r.o.StateDir, "artifacts"), r.b.Artifacts, retry); err != nil {
		return err
	}
	if len(r.b.GitHistory) > 0 {
		history := filepath.Join(r.o.StateDir, "artifacts", "source-history", r.id+".bundle")
		if err := (&ledger.FileDocument{Path: history}).Save(r.b.GitHistory); err != nil {
			return err
		}
	}
	materials, err := material.Open(filepath.Join(r.o.StateDir, "materials"), book)
	if err != nil {
		return err
	}
	defer materials.Close()
	// Blob files are content addressed and inert until their metadata commits.
	return materials.StageBlobs(r.b.Material)
}

// installMemory writes the project's memory files, accepting a file an
// interrupted install already wrote with the same bytes.
func (r importRun) installMemory() error {
	for name, raw := range r.b.Memory {
		if name != r.id+".md" && name != r.id+".md.requests.jsonl" && name != r.id+".md.pending.json" && name != r.id+".audit.jsonl" {
			return errors.New("invalid memory path")
		}
		path := filepath.Join(r.o.StateDir, "memory", "projects", name)
		if old, err := os.ReadFile(path); err == nil {
			if string(old) != string(raw) {
				return errors.New("target project memory changed")
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := (&ledger.FileDocument{Path: path}).Save(raw); err != nil {
			return err
		}
	}
	return nil
}

// installedPaths lists everything the install wrote that must be durable
// before the ledger records the project as present.
func (r importRun) installedPaths(marker string) []string {
	paths := []string{r.targetHome, marker}
	if len(r.b.Artifacts.GitBundle) > 0 {
		paths = append(paths, filepath.Join(r.o.StateDir, "artifacts", "objects", r.id+".git"))
	}
	if len(r.b.GitHistory) > 0 {
		paths = append(paths, filepath.Join(r.o.StateDir, "artifacts", "source-history", r.id+".bundle"))
	}
	for blob := range r.b.Material.Blobs {
		paths = append(paths, filepath.Join(r.o.StateDir, "materials", blob))
	}
	for name := range r.b.Memory {
		paths = append(paths, filepath.Join(r.o.StateDir, "memory", "projects", name))
	}
	return paths
}
