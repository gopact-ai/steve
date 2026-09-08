package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// Manifest is what the ledger knows about an artifact: a commit in the
// project's shadow repository, where it came from, and its label.
type Manifest struct {
	ID        string        `json:"id"`
	Project   string        `json:"project"`
	Parent    string        `json:"parent,omitempty"`
	Label     project.Level `json:"label"`
	By        string        `json:"by,omitempty"`
	Message   string        `json:"message,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	// Canonical marks a snapshot of the canonical workspace itself: the
	// merge base for whatever descends from it. Workspace names the copy
	// a snapshot was taken in; such a snapshot descends from the copy's
	// own previous one, never from canonical, and is not landed.
	Canonical bool   `json:"canonical,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// Receipts say where the artifact is durably held. It counts as durable
	// once one of the project's durable places has signed for it.
	Receipts   []Receipt                `json:"receipts,omitempty"`
	Content    *contentreplica.Manifest `json:"content,omitempty"`
	Protection string                   `json:"protection,omitempty"`
}

// Receipt is a place's word that it holds the artifact.
type Receipt struct {
	Place string    `json:"place"`
	At    time.Time `json:"at"`
}

// Durable reports whether one of the project's durable places holds it.
func (m Manifest) Durable(p project.Project) bool {
	if m.Content != nil && !m.Content.Complete() {
		return false
	}
	for _, r := range m.Receipts {
		if p.Durable(r.Place) {
			return true
		}
	}
	return false
}

// Nodes is what the store needs from the node registry: typed artifact
// operations, blob transfer, and the node's git and placement facts.
type Nodes interface {
	Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error)
	PutBlob(ctx context.Context, node, name string, content io.Reader, size int64) error
	GetBlob(ctx context.Context, node, name string, into io.Writer) error
	Git(ctx context.Context, node string) (version string, root string, state string, err error)
	// Level is the data level the hub assigned the node ("" is the hub).
	Level(ctx context.Context, node string) (string, error)
	// Region is whose leases the node's resources carry ("" is the hub's).
	Region(ctx context.Context, node string) (string, error)
}

// Store holds every project's shadow repository on the hub — the default
// durable place — and materialises workspaces anywhere.
type Store struct {
	Review           ReviewLimits
	landingDriverTTL time.Duration
	// renewTicks paces landing-driver renewals; nil means a real ticker.
	renewTicks   func(time.Duration) (<-chan time.Time, func())
	executions   *execution.Registry
	Dir          string
	Limits       Limits
	ledger       *ledger.Ledger
	projects     *project.Store
	nodes        Nodes
	now          func() time.Time
	replication  contentreplica.Replicator
	contentState contentReplicationState
	// ContentLimits bounds verification of a complete replicated history,
	// independently of Limits, which bounds the current workspace snapshot.
	ContentLimits ContentLimits
	// LegacyMerge forces the pre-2.38 merge path at nodes; tests use it
	// to exercise that path on a modern git.
	LegacyMerge bool
	// Direct lets a node fetch an artifact from another node that holds
	// it, the hub granting the transfer, instead of relaying the bytes.
	Direct bool
	// shadows names the shadow repositories known to be initialised on a
	// node, by the node generation that was asked.
	shadowMu sync.Mutex
	shadows  map[string]int64
}

func (s *Store) SetExecution(r *execution.Registry) { s.executions = r }

// SetReplication is wired with the active generation's ledger writer.
func (s *Store) SetReplication(r contentreplica.Replicator) { s.replication = r }

func New(dir string, l *ledger.Ledger, projects *project.Store, nodes Nodes) *Store {
	return &Store{Dir: dir, ledger: l, projects: projects, nodes: nodes, now: time.Now}
}

const manifestKind = "artifact"

// Project reads a project's record.
func (s *Store) Project(ctx context.Context, id string) (project.Project, bool, error) {
	return s.projects.Get(ctx, id)
}

// admits checks that a node may hold the project's data: the node's level
// must reach the project's, and a sealed project never leaves its home.
func (s *Store) admits(ctx context.Context, p project.Project, node string) error {
	if p.Level == project.LevelSealed && node != p.Home.Node {
		return fmt.Errorf("project %s is sealed: it runs only at its home, %s", p.ID, placeName(p.Home.Node))
	}
	level, err := s.nodes.Level(ctx, node)
	if err != nil {
		return err
	}
	if !p.Level.OrDefault().Admits(project.Level(level).OrDefault()) {
		return fmt.Errorf("project %s is %s; %s is only %s", p.ID, p.Level.OrDefault(), placeName(node), project.Level(level).OrDefault())
	}
	return nil
}

func placeName(node string) string {
	if node == "" {
		return "the hub"
	}
	return node
}

// metadataOnly says the hub keeps no objects of the project: sealed data
// stays at its home node, which is its durable place.
func metadataOnly(p project.Project) bool {
	return p.Level == project.LevelSealed && p.Home.Node != ""
}

// Repo opens the project's shadow repository on the hub.
func (s *Store) Repo(ctx context.Context, projectID string) (*Repo, error) {
	objectsAllowed := true
	if s.replication != nil {
		p, ok, err := s.projects.GetHistorical(ctx, projectID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, project.ErrUnknown
		}
		objectsAllowed = !metadataOnly(p)
		if ok && !metadataOnly(p) {
			if _, err := s.replication.CheckLocal(ctx, projectID); err != nil {
				return nil, err
			}
		}
	}
	r, err := Open(ctx, filepath.Join(s.Dir, "objects", projectID+".git"))
	if err == nil {
		r.Limits = s.Limits
		if s.replication != nil && objectsAllowed {
			err = s.restoreContent(ctx, projectID, r)
		}
	}
	return r, err
}

// Manifest reads an artifact's record.
func (s *Store) Manifest(ctx context.Context, id string) (Manifest, bool, error) {
	var m Manifest
	ok, err := s.ledger.GetBinding(ctx, manifestKind, id, &m)
	if err == nil && ok && m.Content != nil {
		current, found, loadErr := contentreplica.Lookup(ctx, s.ledger, m.Content.ID)
		if loadErr != nil {
			return Manifest{}, false, loadErr
		}
		if !found {
			return Manifest{}, false, contentreplica.ErrIncomplete
		}
		if current.Object.Scope.ProjectID != m.Project || current.Object.Kind != contentreplica.GitBundle || current.Object.Key != m.ID {
			return Manifest{}, false, contentreplica.ErrIntegrity
		}
		m.Content, m.Protection = &current, current.Protection
	}
	return m, ok, err
}

// record writes a manifest, keeping receipts already there.
func (s *Store) record(ctx context.Context, m Manifest) (Manifest, error) {
	if existing, ok, err := s.Manifest(ctx, m.ID); err != nil {
		return m, err
	} else if ok {
		m.Receipts = append(existing.Receipts, m.Receipts...)
		if m.CreatedAt.IsZero() {
			m.CreatedAt = existing.CreatedAt
		}
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = s.now().UTC()
	}
	if s.replication != nil {
		p, ok, err := s.projects.GetHistorical(ctx, m.Project)
		if err != nil {
			return m, err
		}
		if !ok {
			return m, project.ErrUnknown
		}
		if metadataOnly(p) {
			m.Protection = contentreplica.SealedHome
			m.Content = nil
		} else {
			manifest, err := s.prepareContent(ctx, m)
			if err != nil {
				return m, err
			}
			m.Content = &manifest
			m.Protection = manifest.Protection
		}
	}
	err := s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		if m.Content != nil {
			manifest, err := contentreplica.Record(tx, *m.Content)
			if err != nil {
				return err
			}
			m.Content = &manifest
			m.Protection = manifest.Protection
			if err := tx.PutBinding(artifactContentKind, m.Project+"/"+m.ID, manifest.ID); err != nil {
				return err
			}
		}
		return tx.PutBinding(manifestKind, m.ID, m)
	})
	return m, err
}

// hubReceipt signs for an artifact now held in the hub's repository.
func (s *Store) hubReceipt(ctx context.Context, m Manifest) (Manifest, error) {
	m.Receipts = []Receipt{{Place: "", At: s.now().UTC()}}
	return s.record(ctx, m)
}

// receipt signs where the artifact is actually held: the hub, or — for a
// sealed project — its home node, the only durable place it has.
func (s *Store) receipt(ctx context.Context, p project.Project, m Manifest) (Manifest, error) {
	if metadataOnly(p) {
		m.Receipts = []Receipt{{Place: p.Home.Node, At: s.now().UTC()}}
		return s.record(ctx, m)
	}
	return s.hubReceipt(ctx, m)
}

// ---------------------------------------------------------------- canonical

// SnapshotCanonical takes a before- or after-snapshot of the project's
// canonical workspace wherever it lives, brings it to the hub, and records
// it. parent is the previous canonical snapshot; the artifact returned is
// parent itself when nothing changed.
func (s *Store) SnapshotCanonical(ctx context.Context, p project.Project, parent, by, message string) (Manifest, bool, error) {
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return Manifest{}, false, err
	}
	var sha string
	var changed bool
	if p.Home.Node == "" {
		sha, changed, err = repo.Snapshot(ctx, p.Home.Path, parent, message, false)
		if err != nil {
			return Manifest{}, false, err
		}
	} else {
		sha, changed, err = s.snapshotOnNode(ctx, p.Home.Node, p, p.Home.Path, parent, message, repo, false)
		if err != nil {
			return Manifest{}, false, err
		}
	}
	if !changed {
		m, ok, err := s.Manifest(ctx, sha)
		if err == nil && ok {
			if s.replication != nil {
				m, err = s.receipt(ctx, p, m)
			}
			return m, false, err
		}
	}
	m, err := s.receipt(ctx, p, Manifest{ID: sha, Project: p.ID, Parent: parent, Label: p.Level, By: by, Message: message, Canonical: true})
	if err != nil {
		return m, changed, err
	}
	// The snapshot is now the project's last known canonical state.
	return m, changed, s.setCanonical(ctx, p.ID, sha)
}

// SnapshotWorkspace takes a before- or after-snapshot of a copy, wherever
// it is, brings it to the hub, and records it against the copy. parent is
// the copy's previous snapshot; the artifact returned is parent itself
// when nothing changed. It never moves the project's canonical name: a
// copy's history is its own, and only what an isolated step publishes
// enters the landing graph.
func (s *Store) SnapshotWorkspace(ctx context.Context, p project.Project, ws project.Workspace, parent, by, message string) (Manifest, bool, error) {
	if ws.Kind != project.KindCopy {
		return Manifest{}, false, fmt.Errorf("artifact: %s is not a copy", ws.ID)
	}
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return Manifest{}, false, err
	}
	var sha string
	var changed bool
	if ws.Node == "" {
		sha, changed, err = repo.Snapshot(ctx, ws.Path, parent, message, false)
	} else {
		sha, changed, err = s.snapshotOnNode(ctx, ws.Node, p, ws.Path, parent, message, repo, false)
	}
	if err != nil {
		return Manifest{}, false, err
	}
	if !changed {
		if m, ok, err := s.Manifest(ctx, sha); err == nil && ok {
			if s.replication != nil {
				m, err = s.receipt(ctx, p, m)
			}
			return m, false, err
		}
	}
	m, err := s.receipt(ctx, p, Manifest{ID: sha, Project: p.ID, Parent: parent, Label: p.Level, By: by, Message: message, Workspace: ws.ID})
	if err != nil {
		return m, changed, err
	}
	return m, changed, s.setHead(ctx, CopyRef(ws.ID), sha)
}

// CopyRef names a copy's last known snapshot.
func CopyRef(workspaceID string) string { return "workspace/" + workspaceID + "/head" }

// HeadOf is the last known snapshot of a copy, or "".
func (s *Store) HeadOf(ctx context.Context, workspaceID string) string {
	ref, ok, err := s.ledger.Name(ctx, CopyRef(workspaceID))
	if err != nil || !ok {
		return ""
	}
	return ref.Artifact
}

func (s *Store) setCanonical(ctx context.Context, projectID, sha string) error {
	return s.setHead(ctx, CanonicalRef(projectID), sha)
}

// setHead moves a name to sha under compare-and-set.
func (s *Store) setHead(ctx context.Context, name, sha string) error {
	// Two attempts may snapshot the same directory at the same moment and
	// race to name the result; the same content loses nothing by losing
	// the race, and different content is retried against the fresh version.
	for i := 0; i < 5; i++ {
		current, _, err := s.ledger.Name(ctx, name)
		if err != nil {
			return err
		}
		if current.Artifact == sha {
			return nil
		}
		_, err = s.Bind(ctx, name, current.Version, sha)
		if err == nil || !errors.Is(err, ledger.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("name %s kept moving", name)
}

// snapshotOnNode snapshots a directory on a node into the node's shadow
// repository and fetches the result to the hub.
func (s *Store) snapshotOnNode(ctx context.Context, node string, p project.Project, dir, parent, message string, hub *Repo, flatten bool) (string, bool, error) {
	_, _, state, err := s.nodes.Git(ctx, node)
	if err != nil {
		return "", false, err
	}
	bare := nodeBare(state, p.ID)
	sha, changed, trusted, err := s.snapshotOnNodeWith(ctx, node, p, dir, parent, message, hub, flatten, bare, true)
	if err == nil || !trusted || ctx.Err() != nil {
		return sha, changed, err
	}
	var tooLarge TooLarge
	if errors.As(err, &tooLarge) {
		return "", false, err
	}
	// What was trusted — the shadow repository, the parent's replica — may
	// be gone from the node after all: look, and take the snapshot again.
	log.Printf("artifact: snapshot on %s failed after trusting its state (%v); checking the node", node, err)
	s.forgetShadow(node, bare)
	sha, changed, _, err = s.snapshotOnNodeWith(ctx, node, p, dir, parent, message, hub, flatten, bare, false)
	return sha, changed, err
}

// snapshotOnNodeWith takes the snapshot on the node. With trust, the shadow
// repository this generation already initialised and a parent whose
// replica is verified there are not asked about again: on a distant node
// each question is a round trip, and an unchanged snapshot used to cost
// three of them. trusted reports whether anything was skipped that way.
func (s *Store) snapshotOnNodeWith(ctx context.Context, node string, p project.Project, dir, parent, message string, hub *Repo, flatten bool, bare string, trust bool) (sha string, changed, trusted bool, err error) {
	gen := s.generationOf(ctx, node)
	if trust && s.shadowKnown(node, bare, gen) {
		trusted = true
	} else {
		if _, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Init, Repo: bare}); err != nil {
			return "", false, trusted, fmt.Errorf("init shadow repo on %s: %w", node, err)
		}
		s.rememberShadow(node, bare, gen)
	}
	if parent != "" {
		if r, ok := s.replica(ctx, parent, node); ok && r.State == ReplicaVerified && r.Generation == gen {
			if trust {
				trusted = true
			} else {
				// The record said the node had it; the snapshot said otherwise.
				s.setReplica(ctx, parent, node, gen, ReplicaQuarantined, "not found by a snapshot")
			}
		}
		if err := s.ensureOnNode(ctx, p, node, bare, hub, parent); err != nil {
			return "", false, trusted, err
		}
	}
	result, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Snapshot, Repo: bare, WorkTree: dir, Parent: parent, Message: message, Flatten: flatten, Limits: ops.Limits(s.Limits)})
	if err != nil {
		var tooLarge TooLarge
		if errors.As(err, &tooLarge) {
			return "", false, trusted, tooLarge
		}
		return "", false, trusted, fmt.Errorf("snapshot %s on %s: %w", dir, node, err)
	}
	sha = result.Commit
	if !shaPattern.MatchString(sha) {
		return "", false, trusted, fmt.Errorf("snapshot on %s returned %q", node, sha)
	}
	if !result.Changed {
		return sha, false, trusted, nil
	}
	s.setReplica(ctx, sha, node, gen, ReplicaVerified, "made here")
	if metadataOnly(p) {
		return sha, true, trusted, nil
	}
	if err := s.pull(ctx, node, bare, hub, sha, []string{parent}); err != nil {
		return "", false, trusted, err
	}
	return sha, true, trusted, nil
}

// shadowKnown reports whether the node's shadow repository was initialised
// by this hub process while the node ran this generation.
func (s *Store) shadowKnown(node, bare string, gen int64) bool {
	s.shadowMu.Lock()
	defer s.shadowMu.Unlock()
	known, ok := s.shadows[node+"\x00"+bare]
	return ok && known == gen
}

func (s *Store) rememberShadow(node, bare string, gen int64) {
	s.shadowMu.Lock()
	defer s.shadowMu.Unlock()
	if s.shadows == nil {
		s.shadows = map[string]int64{}
	}
	s.shadows[node+"\x00"+bare] = gen
}

func (s *Store) forgetShadow(node, bare string) {
	s.shadowMu.Lock()
	defer s.shadowMu.Unlock()
	delete(s.shadows, node+"\x00"+bare)
}

func (s *Store) nodeHas(ctx context.Context, node, bare, sha string) bool {
	result, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Has, Repo: bare, Commit: sha})
	return err == nil && result.Has
}

// push carries an artifact hub → node as a bundle.
func (s *Store) push(ctx context.Context, node, bare string, hub *Repo, sha string, have []string) error {
	temp, err := os.CreateTemp("", "steve-push-*.bundle")
	if err != nil {
		return err
	}
	path := temp.Name()
	temp.Close()
	defer os.Remove(path)
	if err := hub.Bundle(ctx, path, sha, have); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, _ := file.Stat()
	name := "push-" + short(sha) + ".bundle"
	if err := s.nodes.PutBlob(ctx, node, name, file, info.Size()); err != nil {
		return err
	}
	_, _, state, err := s.nodes.Git(ctx, node)
	if err != nil {
		return err
	}
	remote := filepath.Join(state, "blobs", name)
	if _, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Unbundle, Repo: bare, Path: remote}); err != nil {
		return fmt.Errorf("unbundle on %s: %w", node, err)
	}
	// The bundle has been unpacked; one left behind costs disk, not
	// correctness, so its removal does not fail the push.
	_, _ = s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Remove, Path: remote})
	return nil
}

// pull carries an artifact node → hub as a bundle.
func (s *Store) pull(ctx context.Context, node, bare string, hub *Repo, sha string, have []string) error {
	_, _, state, err := s.nodes.Git(ctx, node)
	if err != nil {
		return err
	}
	name := "pull-" + short(sha) + ".bundle"
	remote := filepath.Join(state, "blobs", name)
	if _, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Bundle, Repo: bare, Path: remote, Commit: sha, Have: have}); err != nil {
		return fmt.Errorf("bundle on %s: %w", node, err)
	}
	temp, err := os.CreateTemp("", "steve-pull-*.bundle")
	if err != nil {
		return err
	}
	path := temp.Name()
	defer os.Remove(path)
	if err := s.nodes.GetBlob(ctx, node, name, temp); err != nil {
		temp.Close()
		return err
	}
	temp.Close()
	// The bundle has been fetched; one left behind costs disk, not
	// correctness, so its removal does not fail the pull.
	_, _ = s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Remove, Path: remote})
	return hub.Unbundle(ctx, path)
}

func nodeBare(state, projectID string) string {
	return filepath.Join(state, "objects", projectID+".git")
}

// ---------------------------------------------------------------- workspaces

// Materialize is the seam every executor asks through. Canonical requests
// go to the project store; isolated ones get a fresh directory from the
// base artifact, on the hub or on the node, with inputs alongside.
func (s *Store) Materialize(ctx context.Context, req project.Request) (project.Workspace, error) {
	if !req.Isolated {
		return s.projects.Materialize(ctx, req)
	}
	p, ok, err := s.projects.Get(ctx, req.Project)
	if err != nil {
		return project.Workspace{}, err
	}
	if !ok {
		return project.Workspace{}, fmt.Errorf("%w: %s", project.ErrUnknown, req.Project)
	}
	if err := s.admits(ctx, p, req.Node); err != nil {
		return project.Workspace{}, err
	}
	base := req.Base
	if base == "" {
		m, _, err := s.SnapshotCanonical(ctx, p, s.canonicalRef(ctx, p.ID), req.Owner, "base for "+req.Owner)
		if err != nil {
			return project.Workspace{}, fmt.Errorf("base snapshot: %w", err)
		}
		base = m.ID
	}
	hub, err := s.Repo(ctx, p.ID)
	if err != nil {
		return project.Workspace{}, err
	}
	if !metadataOnly(p) && !hub.Has(ctx, base) {
		return project.Workspace{}, fmt.Errorf("artifact %s is not on the hub", short(base))
	}
	id := "wt-" + short(base) + "-" + strings.TrimPrefix(req.Owner, "att-")
	if req.Owner == "" {
		id = "wt-" + short(base) + "-" + fmt.Sprint(s.now().UnixNano())
	}
	if req.Node == "" {
		dir := filepath.Join(s.Dir, "worktrees", id)
		if err := hub.Checkout(ctx, base, dir); err != nil {
			return project.Workspace{}, err
		}
		for _, in := range req.Inputs {
			if err := hub.Checkout(ctx, in.Artifact, filepath.Join(dir, "inputs", in.Name)); err != nil {
				return project.Workspace{}, fmt.Errorf("input %s: %w", in.Name, err)
			}
		}
		return project.Workspace{ID: id, Project: p.ID, Node: "", Path: dir, Kind: project.KindWorktree, Base: base}, nil
	}
	version, root, state, err := s.nodes.Git(ctx, req.Node)
	if err != nil {
		return project.Workspace{}, err
	}
	if version == "" {
		return project.Workspace{}, fmt.Errorf("node %s has no git; it cannot hold a workspace for %s", req.Node, p.ID)
	}
	bare := nodeBare(state, p.ID)
	if _, err := s.nodes.Artifact(ctx, req.Node, ops.Request{Op: ops.Init, Repo: bare}); err != nil {
		return project.Workspace{}, fmt.Errorf("init shadow repo on %s: %w", req.Node, err)
	}
	wanted := append([]string{base}, inputArtifacts(req.Inputs)...)
	for _, sha := range wanted {
		if err := s.ensureOnNode(ctx, p, req.Node, bare, hub, sha); err != nil {
			return project.Workspace{}, err
		}
	}
	dir := filepath.Join(root, "worktrees", id)
	if _, err := s.nodes.Artifact(ctx, req.Node, ops.Request{Op: ops.Checkout, Repo: bare, Commit: base, WorkTree: dir}); err != nil {
		return project.Workspace{}, fmt.Errorf("checkout on %s: %w", req.Node, err)
	}
	for _, in := range req.Inputs {
		if _, err := s.nodes.Artifact(ctx, req.Node, ops.Request{Op: ops.Checkout, Repo: bare, Commit: in.Artifact, WorkTree: filepath.Join(dir, "inputs", in.Name)}); err != nil {
			return project.Workspace{}, fmt.Errorf("input %s on %s: %w", in.Name, req.Node, err)
		}
	}
	return project.Workspace{ID: id, Project: p.ID, Node: req.Node, Path: dir, Kind: project.KindWorktree, Base: base}, nil
}

func inputArtifacts(inputs []project.Input) []string {
	out := make([]string, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, in.Artifact)
	}
	return out
}

// Publish snapshots a workspace after an attempt, brings the result to the
// hub, and records it with the hub's receipt. inputs/ is not part of the
// result: it was given, not made.
func (s *Store) Publish(ctx context.Context, ws project.Workspace, parent, by, message string) (Manifest, bool, error) {
	p, ok, err := s.projects.Get(ctx, ws.Project)
	if err != nil {
		return Manifest{}, false, err
	}
	if !ok {
		return Manifest{}, false, fmt.Errorf("%w: %s", project.ErrUnknown, ws.Project)
	}
	hub, err := s.Repo(ctx, p.ID)
	if err != nil {
		return Manifest{}, false, err
	}
	var sha string
	var changed bool
	if ws.Node == "" {
		s.dropInputs(ws.Path)
		// The platform's own worktree: whatever repository an agent started
		// inside it is flattened so the files come through.
		sha, changed, err = hub.Snapshot(ctx, ws.Path, parent, message, ws.Kind == project.KindWorktree)
	} else {
		// Like dropInputs on the hub: inputs are dropped when possible so
		// they do not enter the snapshot, and a leftover is not an error.
		_, _ = s.nodes.Artifact(ctx, ws.Node, ops.Request{Op: ops.Remove, Path: filepath.Join(ws.Path, "inputs")})
		sha, changed, err = s.snapshotOnNode(ctx, ws.Node, p, ws.Path, parent, message, hub, ws.Kind == project.KindWorktree)
	}
	if err != nil {
		return Manifest{}, false, err
	}
	if !changed {
		m, ok, err := s.Manifest(ctx, sha)
		if err == nil && ok {
			if s.replication != nil {
				m, err = s.receipt(ctx, p, m)
			}
			return m, false, err
		}
	}
	m, err := s.receipt(ctx, p, Manifest{ID: sha, Project: p.ID, Parent: parent, Label: p.Level, By: by, Message: message})
	return m, changed, err
}

// dropInputs clears a workspace's inputs before a snapshot. There may be
// none, and a directory that cannot be removed is flattened into the
// snapshot as any other file would be.
func (s *Store) dropInputs(dir string) {
	_ = os.RemoveAll(filepath.Join(dir, "inputs"))
}

// Discard removes a workspace once its attempt is over.
func (s *Store) Discard(ctx context.Context, ws project.Workspace) error {
	if ws.Kind != project.KindWorktree {
		return errors.New("artifact: only worktrees are discarded")
	}
	if ws.Node == "" {
		return os.RemoveAll(ws.Path)
	}
	_, err := s.nodes.Artifact(ctx, ws.Node, ops.Request{Op: ops.Remove, Path: ws.Path})
	return err
}

// ---------------------------------------------------------------- names

// CanonicalRef names the project's last known canonical snapshot.
func CanonicalRef(projectID string) string { return "project/" + projectID + "/canonical" }

// CanonicalOf is the project's last known canonical snapshot, or "".
func (s *Store) CanonicalOf(ctx context.Context, projectID string) string {
	return s.canonicalRef(ctx, projectID)
}

func (s *Store) canonicalRef(ctx context.Context, projectID string) string {
	ref, ok, err := s.ledger.Name(ctx, CanonicalRef(projectID))
	if err != nil || !ok {
		return ""
	}
	return ref.Artifact
}

// Bind points a name at an artifact under compare-and-set on its version.
func (s *Store) Bind(ctx context.Context, name string, expectedVersion int64, artifact string) (int64, error) {
	var version int64
	err := s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		v, err := tx.CompareAndSetName(name, expectedVersion, artifact)
		version = v
		return err
	})
	return version, err
}

// Resolve reads a name.
func (s *Store) Resolve(ctx context.Context, name string) (ledger.NamedRef, bool, error) {
	return s.ledger.Name(ctx, name)
}

// ---------------------------------------------------------------- deferred landings

const pendingKind = "pending-landing"

// Pending is an artifact waiting to land once the canonical lock frees:
// what a delegated child made while its parent's in-place turn still held
// the lock.
type Pending struct {
	Source   *Source   `json:"source,omitempty"`
	Project  string    `json:"project"`
	Artifact string    `json:"artifact"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
}

// Defer queues an artifact to land later.
func (s *Store) Defer(ctx context.Context, projectID, artifactID, by string, source ...Source) error {
	origin := firstSource(source)
	return s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		if origin != nil {
			if err := task.CheckExecutionTx(tx, origin.Execution); err != nil {
				return err
			}
		}
		return tx.PutBinding(pendingKind, projectID+"/"+artifactID, Pending{Project: projectID, Artifact: artifactID, By: by, At: s.now().UTC(), Source: origin})
	})
}

// LandPending lands everything queued for the project, oldest first, and
// returns each landing. A conflict stops that artifact but not the rest;
// conflicted artifacts stay queued for a person to resolve.
func (s *Store) LandPending(ctx context.Context, p project.Project) ([]Landing, error) {
	raw, err := s.ledger.Bindings(ctx, pendingKind)
	if err != nil {
		return nil, err
	}
	var queue []Pending
	for _, data := range raw {
		var item Pending
		if err := json.Unmarshal(data, &item); err == nil && item.Project == p.ID {
			queue = append(queue, item)
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].At.Before(queue[j].At) })
	var out []Landing
	for _, item := range queue {
		var source []Source
		if item.Source != nil {
			source = []Source{*item.Source}
		}
		land, err := s.Land(ctx, p, item.Artifact, item.By, source...)
		if errors.Is(err, task.ErrExecutionStopped) {
			if err := s.ledger.DeleteBinding(ctx, pendingKind, item.Project+"/"+item.Artifact); err != nil {
				return out, err
			}
			continue
		}
		if err != nil {
			var conflict Conflict
			if errors.As(err, &conflict) {
				out = append(out, land)
				continue
			}
			return out, err
		}
		out = append(out, land)
		// The landing is committed under its durable identity; a pending
		// record that survives is answered from that identity next sweep.
		_ = s.ledger.DeleteBinding(ctx, pendingKind, item.Project+"/"+item.Artifact)
	}
	return out, nil
}

// Changed lists the paths that differ between two of the project's
// artifacts: what a step actually wrote, against what it declared.
func (s *Store) Changed(ctx context.Context, projectID, from, to string) ([]string, error) {
	repo, err := s.Repo(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return repo.Changed(ctx, from, to)
}

// homeRegion is the region of the project's canonical workspace.
func (s *Store) homeRegion(ctx context.Context, p project.Project) string {
	region, err := s.nodes.Region(ctx, p.Home.Node)
	if err != nil {
		return ""
	}
	return region
}
