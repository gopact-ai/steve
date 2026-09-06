package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
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
	Receipts []Receipt `json:"receipts,omitempty"`
}

// Receipt is a place's word that it holds the artifact.
type Receipt struct {
	Place string    `json:"place"`
	At    time.Time `json:"at"`
}

// Durable reports whether one of the project's durable places holds it.
func (m Manifest) Durable(p project.Project) bool {
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
	Dir      string
	Limits   Limits
	ledger   *ledger.Ledger
	projects *project.Store
	nodes    Nodes
	now      func() time.Time
	// LegacyMerge forces the pre-2.38 merge path at nodes; tests use it
	// to exercise that path on a modern git.
	LegacyMerge bool
	// Direct lets a node fetch an artifact from another node that holds
	// it, the hub granting the transfer, instead of relaying the bytes.
	Direct bool
}

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
	r, err := Open(ctx, filepath.Join(s.Dir, "objects", projectID+".git"))
	if err == nil {
		r.Limits = s.Limits
	}
	return r, err
}

// Manifest reads an artifact's record.
func (s *Store) Manifest(ctx context.Context, id string) (Manifest, bool, error) {
	var m Manifest
	ok, err := s.ledger.GetBinding(ctx, manifestKind, id, &m)
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
	return m, s.ledger.PutBinding(ctx, manifestKind, m.ID, m)
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
			return m, false, nil
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
			return m, false, nil
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
	if _, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Init, Repo: bare}); err != nil {
		return "", false, fmt.Errorf("init shadow repo on %s: %w", node, err)
	}
	if parent != "" && !s.nodeHas(ctx, node, bare, parent) {
		if err := s.push(ctx, node, bare, hub, parent, nil); err != nil {
			return "", false, err
		}
	}
	result, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Snapshot, Repo: bare, WorkTree: dir, Parent: parent, Message: message, Flatten: flatten, Limits: ops.Limits(s.Limits)})
	if err != nil {
		var tooLarge TooLarge
		if errors.As(err, &tooLarge) {
			return "", false, tooLarge
		}
		return "", false, fmt.Errorf("snapshot %s on %s: %w", dir, node, err)
	}
	sha := result.Commit
	if !shaPattern.MatchString(sha) {
		return "", false, fmt.Errorf("snapshot on %s returned %q", node, sha)
	}
	if !result.Changed {
		return sha, false, nil
	}
	s.setReplica(ctx, sha, node, s.generationOf(ctx, node), ReplicaVerified, "made here")
	if metadataOnly(p) {
		return sha, true, nil
	}
	if err := s.pull(ctx, node, bare, hub, sha, []string{parent}); err != nil {
		return "", false, err
	}
	return sha, true, nil
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
		_, _ = s.nodes.Artifact(ctx, ws.Node, ops.Request{Op: ops.Remove, Path: filepath.Join(ws.Path, "inputs")})
		sha, changed, err = s.snapshotOnNode(ctx, ws.Node, p, ws.Path, parent, message, hub, ws.Kind == project.KindWorktree)
	}
	if err != nil {
		return Manifest{}, false, err
	}
	if !changed {
		m, ok, err := s.Manifest(ctx, sha)
		if err == nil && ok {
			return m, false, nil
		}
	}
	m, err := s.receipt(ctx, p, Manifest{ID: sha, Project: p.ID, Parent: parent, Label: p.Level, By: by, Message: message})
	return m, changed, err
}

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
	Project  string    `json:"project"`
	Artifact string    `json:"artifact"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
}

// Defer queues an artifact to land later.
func (s *Store) Defer(ctx context.Context, projectID, artifactID, by string) error {
	return s.ledger.PutBinding(ctx, pendingKind, projectID+"/"+artifactID, Pending{Project: projectID, Artifact: artifactID, By: by, At: s.now().UTC()})
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
		land, err := s.Land(ctx, p, item.Artifact, item.By)
		if err != nil {
			var conflict Conflict
			if errors.As(err, &conflict) {
				out = append(out, land)
				continue
			}
			return out, err
		}
		out = append(out, land)
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
