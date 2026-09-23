package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
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
	if m.Content != nil {
		if !m.Content.Complete() || m.Content.Object.Kind != contentreplica.GitBundle || m.Content.Object.Key != m.ID || m.Content.Object.Scope.ProjectID != p.ID {
			return false
		}
		// Cluster configuration pins durable places to physical node IDs.
		// Receiver-verified content receipts carry those IDs; the legacy
		// hub receipt below only identifies the standalone coordinator.
		for _, receipt := range m.Content.Receipts {
			if p.Durable(receipt.NodeID) {
				return true
			}
		}
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
	// Policy supplies one atomic policy snapshot per request. Install it before
	// use and never replace it while the store is running. Nil uses Limits/Review.
	Policy           func() (Limits, ReviewLimits)
	Review           ReviewLimits
	landingDriverTTL time.Duration
	// snapshotLimit bounds a snapshot cut under the canonical lock; zero
	// means canonicalSnapshotLimit.
	snapshotLimit time.Duration
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
	// recoveryNotes holds, by landing, the last reason its recovery was
	// left for a retry, so a reason that repeats every pass is logged once.
	recoveryNotes sync.Map
	// inApply holds the landings this process is applying right now.
	inApply sync.Map
	// writing holds, by project, a one-slot channel taken by whoever in
	// this process writes the canonical workspace or cuts it under a lock
	// it shares: a landing under a lent lock and its lender (see
	// writeCanonical).
	writing sync.Map
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

func (s *Store) policy() (Limits, ReviewLimits) {
	if s.Policy != nil {
		return s.Policy()
	}
	return s.Limits, s.Review
}

// Repo opens the project's shadow repository on the hub. Its policy is copied
// at entry and stays fixed for every operation on the returned repository.
func (s *Store) Repo(ctx context.Context, projectID string) (*Repo, error) {
	limits, review := s.policy()
	return s.repoWithPolicy(ctx, projectID, limits, review)
}

func (s *Store) repoWithPolicy(ctx context.Context, projectID string, limits Limits, review ReviewLimits) (*Repo, error) {
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
		r.Limits, r.Review = limits, review
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

// SnapshotCanonical takes a snapshot of the project's canonical workspace
// wherever it lives, brings it to the hub, records it and moves the
// canonical name to it. It takes the canonical lock for that and gives it
// back: the name only moves under the lock, and nothing else writes the
// workspace while it is cut. A lock someone else holds comes back as
// ledger.ErrHeld, and a workspace an interrupted landing half wrote as
// ErrRecoveryPending. parent is the previous canonical snapshot; the
// artifact returned is parent itself when nothing changed. It records the
// workspace as it is; a base for new work is CanonicalBase, which also
// stands aside for a holder of the lock and a pending recovery.
func (s *Store) SnapshotCanonical(ctx context.Context, p project.Project, parent, by, message string) (Manifest, bool, error) {
	var m Manifest
	var changed bool
	err := s.underCanonical(ctx, p, by, func(ctx context.Context, held ledger.Lease) error {
		var err error
		m, changed, _, err = s.snapshotCanonical(ctx, p, held, parent, by, message)
		return err
	})
	return m, changed, err
}

// SnapshotCanonicalUnder is SnapshotCanonical for a caller already holding
// the canonical lock, which held is: an in-place turn snapshotting the
// directory it works in. The name moves fenced on that lock. A landing
// the holder lent the lock to is waited for; with one waiting for
// recovery, the workspace is half written and the snapshot is the one
// the name is at, unchanged.
func (s *Store) SnapshotCanonicalUnder(ctx context.Context, p project.Project, held ledger.Lease, parent, by, message string) (Manifest, bool, error) {
	var m Manifest
	var changed bool
	err := s.underLent(ctx, p, func() error {
		var err error
		m, changed, _, err = s.snapshotCanonical(ctx, p, held, parent, by, message)
		return err
	})
	if !errors.Is(err, ErrRecoveryPending) {
		return m, changed, err
	}
	head, err := s.namedWhilePending(ctx, p, err)
	if err != nil {
		return Manifest{}, false, err
	}
	m, found, err := s.Manifest(ctx, head)
	if err == nil && !found {
		err = fmt.Errorf("canonical snapshot %s of %s is not recorded", short(head), p.ID)
	}
	return m, false, err
}

// CanonicalBase is the canonical snapshot new work starts from. With the
// canonical lock free it is a fresh snapshot, taken under the lock. With
// the lock held — a landing or an in-place turn is writing the workspace —
// or the workspace half written by a landing waiting for recovery, what is
// on disk is no base: it is the snapshot the canonical name is at, which
// is left where it is. A landing names its snapshot before it writes, so
// with no name yet the holder is not one: the base is then cut as the
// lineage's first snapshot, and naming it is left to the holder — unless
// the holder named one meanwhile, which is then the base.
func (s *Store) CanonicalBase(ctx context.Context, p project.Project, by, message string) (string, error) {
	var base string
	err := s.underCanonical(ctx, p, by, func(ctx context.Context, held ledger.Lease) error {
		var err error
		base, err = s.canonicalBaseUnder(ctx, p, held, by, message)
		return err
	})
	if !errors.Is(err, ledger.ErrHeld) && !errors.Is(err, ErrRecoveryPending) {
		return base, err
	}
	head, readErr := s.CanonicalOf(ctx, p.ID)
	if readErr != nil {
		return "", readErr
	}
	if head != "" {
		return head, nil
	}
	if !errors.Is(err, ledger.ErrHeld) {
		return "", fmt.Errorf("project %s has no canonical snapshot to start from yet: %w", p.ID, err)
	}
	m, _, _, err := s.cutCanonical(ctx, p, "", by, message)
	if err != nil {
		return "", err
	}
	// The holder may have named its first snapshot while the base was cut,
	// and written on: the cut may hold those writes half done, and stands
	// off the lineage the name now starts. The holder's snapshot is the
	// base then.
	if head, err = s.CanonicalOf(ctx, p.ID); err != nil || head != "" {
		return head, err
	}
	return m.ID, nil
}

// CanonicalBaseUnder is CanonicalBase for a caller holding the canonical
// lock, which held is: an in-place turn handing work to a child. What the
// holder has written so far is the base, snapshotted under its lock on top
// of the canonical name, which moves there. A landing the holder lent the
// lock to is waited for; with one waiting for recovery, the workspace is
// half written and the base is the snapshot the name is at, left there.
func (s *Store) CanonicalBaseUnder(ctx context.Context, p project.Project, held ledger.Lease, by, message string) (string, error) {
	var base string
	err := s.underLent(ctx, p, func() error {
		var err error
		base, err = s.canonicalBaseUnder(ctx, p, held, by, message)
		return err
	})
	if errors.Is(err, ErrRecoveryPending) {
		return s.namedWhilePending(ctx, p, err)
	}
	return base, err
}

// canonicalBaseUnder is CanonicalBaseUnder for a caller that has already
// ruled out anyone else writing the workspace under its lock.
func (s *Store) canonicalBaseUnder(ctx context.Context, p project.Project, held ledger.Lease, by, message string) (string, error) {
	parent, err := s.CanonicalOf(ctx, p.ID)
	if err != nil {
		return "", err
	}
	m, _, _, err := s.snapshotCanonical(ctx, p, held, parent, by, message)
	return m.ID, err
}

// underLent runs fn for a holder of the canonical lock once no landing it
// lent the lock to is writing (see writeCanonical), and only if none is
// waiting to be recovered: that is ErrRecoveryPending.
func (s *Store) underLent(ctx context.Context, p project.Project, fn func() error) error {
	done, err := s.writeCanonical(ctx, p.ID)
	if err != nil {
		return err
	}
	defer done()
	if err := s.checkNoRecoveryPending(ctx, p, ""); err != nil {
		return err
	}
	return fn()
}

// namedWhilePending is the snapshot the canonical name is at, which stands
// for the workspace while a landing waiting for recovery has it half
// written. With no name there is nothing to stand for it: pending is
// the error then.
func (s *Store) namedWhilePending(ctx context.Context, p project.Project, pending error) (string, error) {
	head, err := s.CanonicalOf(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if head == "" {
		return "", pending
	}
	return head, nil
}

// writeCanonical takes the project's slot for writing its canonical
// workspace in this process. The canonical lock keeps other holders out,
// but a holder lends its lock to landings, and those write under the same
// lease the holder snapshots under: the slot is what keeps the two apart.
// It waits for the slot as long as ctx allows; the returned func gives it
// back.
func (s *Store) writeCanonical(ctx context.Context, projectID string) (func(), error) {
	slot, _ := s.writing.LoadOrStore(projectID, make(chan struct{}, 1))
	ch := slot.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for the canonical workspace of %s: %w", projectID, context.Cause(ctx))
	}
}

// underCanonical runs fn under the project's canonical lock, taken for it
// and given back after, and only once no interrupted landing is waiting
// to be recovered. fn runs under a ctx bounded by canonicalSnapshotLimit.
func (s *Store) underCanonical(ctx context.Context, p project.Project, by string, fn func(ctx context.Context, held ledger.Lease) error) error {
	lease, err := s.acquireCanonical(ctx, p, SnapshotHolder(by))
	if err != nil {
		return err
	}
	defer func() {
		if err := s.ledger.ReleaseAny(context.WithoutCancel(ctx), lease); err != nil {
			slog.Warn(fmt.Sprintf("artifact: release canonical lock of %s after a snapshot: %v", p.ID, err), "project", p.ID, "holder", lease.Holder)
		}
	}()
	limit := s.snapshotLimit
	if limit <= 0 {
		limit = canonicalSnapshotLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	// Renewed on its own even under a landing driver, which renews the
	// lock its landing holds and must go on doing so. Renewal ends with
	// the snapshot's time, even if fn is stuck in a call that ignores ctx:
	// the lock then runs out instead of being held for as long as fn hangs.
	stopRenew := s.renewCanonical(ctx, lease)
	defer stopRenew()
	defer context.AfterFunc(ctx, stopRenew)()
	if err := s.checkNoRecoveryPending(ctx, p, ""); err != nil {
		return err
	}
	return fn(ctx, lease)
}

// canonicalSnapshotLimit bounds a snapshot cut under the canonical lock.
// The lock is renewed while the cut runs, and turns wait it out, so a cut
// that hangs must end by itself rather than hold the project indefinitely.
const canonicalSnapshotLimit = 10 * time.Minute

// SnapshotHolder is a holder of the canonical lock that only cuts a
// snapshot for by, writing nothing.
func SnapshotHolder(by string) string { return "snapshot:" + by + ":" + attempt.NewID() }

// Snapshotting says whether holder, holding a canonical lock, only cuts a
// snapshot: it writes nothing and gives the lock back once the cut is
// done.
func Snapshotting(holder string) bool { return strings.HasPrefix(holder, "snapshot:") }

// canonicalLock is the lock whoever writes a project's canonical workspace
// or moves its canonical name holds.
func canonicalLock(projectID string) string { return "canonical:" + projectID }

// CanonicalLease picks the project's canonical lock out of leases.
func CanonicalLease(leases []ledger.Lease, projectID string) (ledger.Lease, bool) {
	for _, lease := range leases {
		if lease.Key == canonicalLock(projectID) {
			return lease, true
		}
	}
	return ledger.Lease{}, false
}

// snapshotCanonical is SnapshotCanonicalUnder that also names the nested
// git repositories the snapshot left out of the canonical workspace.
func (s *Store) snapshotCanonical(ctx context.Context, p project.Project, held ledger.Lease, parent, by, message string) (Manifest, bool, []string, error) {
	if held.Key != canonicalLock(p.ID) {
		return Manifest{}, false, nil, fmt.Errorf("snapshot of %s under lock %q, not its canonical lock", p.ID, held.Key)
	}
	m, changed, nested, err := s.cutCanonical(ctx, p, parent, by, message)
	if err != nil {
		return m, changed, nested, err
	}
	// The snapshot is now the project's last known canonical state.
	return m, changed, nested, s.setCanonical(ctx, p.ID, held, m.ID)
}

// cutCanonical snapshots the canonical workspace, brings the snapshot to
// the hub and records it, without moving the canonical name.
func (s *Store) cutCanonical(ctx context.Context, p project.Project, parent, by, message string) (Manifest, bool, []string, error) {
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return Manifest{}, false, nil, err
	}
	var sha string
	var changed bool
	var nested []string
	if p.Home.Node == "" {
		sha, changed, nested, err = repo.snapshot(ctx, p.Home.Path, parent, message, false)
	} else {
		sha, changed, nested, err = s.snapshotOnNode(ctx, p.Home.Node, p, p.Home.Path, parent, message, repo, false)
	}
	if err != nil {
		return Manifest{}, false, nil, err
	}
	if !changed {
		m, ok, err := s.Manifest(ctx, sha)
		if err == nil && ok {
			if s.replication != nil {
				m, err = s.receipt(ctx, p, m)
			}
			return m, false, nested, err
		}
	}
	m, err := s.receipt(ctx, p, Manifest{ID: sha, Project: p.ID, Parent: parent, Label: p.Level, By: by, Message: message, Canonical: true})
	return m, changed, nested, err
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
		sha, changed, _, err = s.snapshotOnNode(ctx, ws.Node, p, ws.Path, parent, message, repo, false)
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

// HeadOf is the last known snapshot of a copy, "" when it has none.
func (s *Store) HeadOf(ctx context.Context, workspaceID string) (string, error) {
	return s.nameOf(ctx, CopyRef(workspaceID))
}

// setCanonical moves the canonical name to sha, fenced on the canonical
// lock the caller holds, which is also why it needs no retry: nobody else
// moves the name meanwhile.
func (s *Store) setCanonical(ctx context.Context, projectID string, held ledger.Lease, sha string) error {
	// A lease another region issued cannot be checked inside this
	// ledger's transaction; it is checked just before instead. That
	// leaves a window: the lease may run out between the check and the
	// transaction, a new holder take the lock, and this move land over
	// the new holder's. Only a holder that stopped renewing its lease can
	// fall into it, as a renewed lease does not run out in the moment
	// between check and transaction.
	foreign := held.Region != "" && held.Region != s.ledger.Region()
	if foreign {
		if err := s.ledger.CheckAny(ctx, held); err != nil {
			return err
		}
	}
	return s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		if !foreign {
			if err := tx.CheckLocalLease(held); err != nil {
				return err
			}
		}
		current, _, err := tx.Name(CanonicalRef(projectID))
		if err != nil {
			return fmt.Errorf("read canonical of %s: %w", projectID, err)
		}
		if current.Artifact == sha {
			return nil
		}
		_, err = tx.CompareAndSetName(CanonicalRef(projectID), current.Version, sha)
		return err
	})
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
// repository and fetches the result to the hub. nested names the nested
// git repositories of the directory the snapshot left out.
func (s *Store) snapshotOnNode(ctx context.Context, node string, p project.Project, dir, parent, message string, hub *Repo, flatten bool) (sha string, changed bool, nested []string, err error) {
	_, _, state, err := s.nodes.Git(ctx, node)
	if err != nil {
		return "", false, nil, err
	}
	bare := nodeBare(state, p.ID)
	sha, changed, nested, trusted, err := s.snapshotOnNodeWith(ctx, node, p, dir, parent, message, hub, flatten, bare, true)
	if err == nil || !trusted || ctx.Err() != nil {
		return sha, changed, nested, err
	}
	var tooLarge TooLarge
	if errors.As(err, &tooLarge) {
		return "", false, nil, err
	}
	// What was trusted — the shadow repository, the parent's replica — may
	// be gone from the node after all: look, and take the snapshot again.
	slog.Warn(fmt.Sprintf("artifact: snapshot on %s failed after trusting its state (%v); checking the node", node, err), "node", node, "project", p.ID)
	s.forgetShadow(node, bare)
	sha, changed, nested, _, err = s.snapshotOnNodeWith(ctx, node, p, dir, parent, message, hub, flatten, bare, false)
	return sha, changed, nested, err
}

// snapshotOnNodeWith takes the snapshot on the node. With trust, the shadow
// repository this generation already initialised and a parent whose
// replica is verified there are not asked about again: on a distant node
// each question is a round trip, and an unchanged snapshot used to cost
// three of them. trusted reports whether anything was skipped that way.
func (s *Store) snapshotOnNodeWith(ctx context.Context, node string, p project.Project, dir, parent, message string, hub *Repo, flatten bool, bare string, trust bool) (sha string, changed bool, nested []string, trusted bool, err error) {
	gen := s.generationOf(ctx, node)
	if trust && s.shadowKnown(node, bare, gen) {
		trusted = true
	} else {
		if _, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Init, Repo: bare}); err != nil {
			return "", false, nil, trusted, fmt.Errorf("init shadow repo on %s: %w", node, err)
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
			return "", false, nil, trusted, err
		}
	}
	result, err := s.nodes.Artifact(ctx, node, ops.Request{Op: ops.Snapshot, Repo: bare, WorkTree: dir, Parent: parent, Message: message, Flatten: flatten, Limits: ops.Limits(hub.Limits)})
	if err != nil {
		var tooLarge TooLarge
		if errors.As(err, &tooLarge) {
			return "", false, nil, trusted, tooLarge
		}
		return "", false, nil, trusted, fmt.Errorf("snapshot %s on %s: %w", dir, node, err)
	}
	sha, nested = result.Commit, result.Nested
	if !shaPattern.MatchString(sha) {
		return "", false, nil, trusted, fmt.Errorf("snapshot on %s returned %q", node, sha)
	}
	if !result.Changed {
		return sha, false, nested, trusted, nil
	}
	s.setReplica(ctx, sha, node, gen, ReplicaVerified, "made here")
	if metadataOnly(p) {
		return sha, true, nested, trusted, nil
	}
	if err := s.pull(ctx, node, bare, hub, sha, []string{parent}); err != nil {
		return "", false, nil, trusted, err
	}
	return sha, true, nested, trusted, nil
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
		base, err = s.CanonicalBase(ctx, p, req.Owner, "base for "+req.Owner)
		if err != nil {
			return project.Workspace{}, fmt.Errorf("base snapshot: %w", err)
		}
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
		sha, changed, _, err = s.snapshotOnNode(ctx, ws.Node, p, ws.Path, parent, message, hub, ws.Kind == project.KindWorktree)
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

// CanonicalOf is the project's last known canonical snapshot, "" when it
// has none yet. A name that cannot be read is an error, never "": work
// started from "" would be cut off the canonical lineage.
func (s *Store) CanonicalOf(ctx context.Context, projectID string) (string, error) {
	return s.nameOf(ctx, CanonicalRef(projectID))
}

// nameOf is the artifact a name points at, "" when it is not bound.
func (s *Store) nameOf(ctx context.Context, name string) (string, error) {
	ref, _, err := s.ledger.Name(ctx, name)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return ref.Artifact, nil
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
	// Blocked records the conflict this artifact last stopped at — a merge
	// conflict, or one found applying the merge to the canonical workspace.
	// Retrying against an unchanged canonical would reach the same
	// conflict, so the queue waits for the canonical name to move —
	// which is what resolving the conflict does.
	Blocked *Blocked `json:"blocked,omitempty"`
}

// Blocked is why a queued artifact is not being retried, and what a
// resolver needs to work from.
type Blocked struct {
	Landing string `json:"landing"`
	// State is the landing state it stopped at: merge-conflicted or
	// apply-conflicted. An older record without one was a merge conflict.
	State     string    `json:"state,omitempty"`
	Canonical string    `json:"canonical"`
	Marked    string    `json:"marked,omitempty"`
	Paths     []string  `json:"paths,omitempty"`
	At        time.Time `json:"at"`
	// Reason says why, in words a user can act on. A merge conflict has a
	// marked tree to resolve from; an apply conflict has only this.
	Reason string `json:"reason,omitempty"`
	// Attempt is the task of a resolution already tried against this
	// canonical. One automatic try per conflict: a second would repeat
	// whatever went wrong, at the cost of an agent run each time.
	Attempt string `json:"attempt,omitempty"`
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
		id := projectID + "/" + artifactID
		item := Pending{Project: projectID, Artifact: artifactID, By: by, At: s.now().UTC(), Source: origin}
		// Deferring a result already queued — one its landing just stopped
		// on a conflict — keeps its place in line and why it is stuck.
		raw, err := tx.Bindings(pendingKind)
		if err != nil {
			return err
		}
		if existing, ok := raw[id]; ok {
			var prior Pending
			if err := json.Unmarshal(existing, &prior); err == nil && prior.Project == projectID {
				item.At, item.Blocked = prior.At, prior.Blocked
			}
		}
		return tx.PutBinding(pendingKind, id, item)
	})
}

// LandPending lands everything queued for the project, oldest first, and
// returns each landing. A conflict stops that artifact but not the rest:
// it stays queued, marked with the canonical snapshot it could not merge
// onto, and is left alone until that canonical moves. Without that the
// sweeper would recompute the same conflict every time it ran and write a
// failed landing for each pass.
func (s *Store) LandPending(ctx context.Context, p project.Project) ([]Landing, error) {
	return s.landPending(ctx, p, nil)
}

// LandPendingUnder is LandPending for a caller that legitimately holds the
// canonical lock — a parent turn composing its prompt, taking what its
// children queued into the directory it is about to work in. Each landing
// is fenced on that lease, as LandUnder is, and releases nothing.
func (s *Store) LandPendingUnder(ctx context.Context, p project.Project, held ledger.Lease) ([]Landing, error) {
	return s.landPending(ctx, p, &held)
}

func (s *Store) landPending(ctx context.Context, p project.Project, held *ledger.Lease) ([]Landing, error) {
	// Turns, delivery callbacks and the background sweep can all drain this
	// queue. One renewed, fenced driver must own its read/land/delete cycle.
	ttl := s.landingDriverTTL
	if ttl <= 0 {
		ttl = landTTL
	}
	drive, err := s.ledger.Acquire(ctx, "pending-landings:"+p.ID, attempt.NewID(), ttl)
	if err != nil {
		return nil, err
	}
	ctx, stop := s.startLandingDriver(ctx, drive, ttl)
	defer stop()
	raw, err := s.ledger.Bindings(ctx, pendingKind)
	if err != nil {
		return nil, err
	}
	remove := func(item Pending) error {
		id := item.Project + "/" + item.Artifact
		return s.ledger.Update(ctx, func(tx *ledger.Tx) error {
			if err := tx.CheckLocalLease(drive); err != nil {
				return err
			}
			// A newer deferral of this artifact retains its own source authority.
			_, err := tx.Exec(`DELETE FROM bindings WHERE kind = ? AND id = ? AND data = ?`, pendingKind, id, string(raw[id]))
			return err
		})
	}
	var queue []Pending
	for _, data := range raw {
		var item Pending
		if err := json.Unmarshal(data, &item); err == nil && item.Project == p.ID {
			queue = append(queue, item)
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].At.Before(queue[j].At) })
	head, err := s.CanonicalOf(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var out []Landing
	for _, item := range queue {
		if item.Blocked != nil && s.stillBlocked(ctx, p, *item.Blocked, head) {
			continue
		}
		var source []Source
		if item.Source != nil {
			source = []Source{*item.Source}
		}
		land, err := s.land(ctx, p, item.Artifact, item.By, held, firstSource(source), nil)
		if errors.Is(err, task.ErrExecutionStopped) {
			if err := remove(item); err != nil {
				return out, err
			}
			continue
		}
		if err != nil {
			var conflict Conflict
			if errors.As(err, &conflict) {
				// The landing recorded why it is stuck on the queue entry
				// itself, which is what keeps it from being retried against
				// the same canonical every pass.
				out = append(out, land)
				continue
			}
			return out, err
		}
		out = append(out, land)
		if err := remove(item); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Stuck is a queued result whose landing stopped at a conflict and is
// waiting for it to be resolved or for the canonical to move.
type Stuck struct {
	Project   string    `json:"project"`
	State     string    `json:"state,omitempty"`
	Artifact  string    `json:"artifact"`
	By        string    `json:"by"`
	Landing   string    `json:"landing"`
	Canonical string    `json:"canonical"`
	Marked    string    `json:"marked,omitempty"`
	Paths     []string  `json:"paths,omitempty"`
	At        time.Time `json:"at"`
	Attempt   string    `json:"attempt,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// Resolvable says whether an agent can be handed a checkout of this
// conflict: git has to have kept the marked tree.
func (s Stuck) Resolvable() bool { return s.Marked != "" }

// stillBlocked says a queued result is still held up by the conflict it
// stopped at, given the canonical head now.
//
// A merge conflict is retried once the canonical moves, which is what
// resolving it does. An apply conflict is retried only once one of its
// own paths changed in the canonical: the canonical also moves for
// reasons that cannot clear it — a landing's own partial writes being
// snapshotted, work elsewhere in the project — and retrying on each of
// those was a loop of full snapshots and merges under the canonical lock.
// A block whose change cannot be read stays a block; Unblock is the way
// out a person has.
func (s *Store) stillBlocked(ctx context.Context, p project.Project, blocked Blocked, head string) bool {
	if blocked.Canonical == head {
		return s.markedStillThere(ctx, blocked)
	}
	if blocked.State != LandApplyConflicted || blocked.Canonical == "" || head == "" || len(blocked.Paths) == 0 {
		return false
	}
	changed, err := s.changedBetween(ctx, p, blocked.Canonical, head)
	if err != nil {
		slog.Warn(fmt.Sprintf("artifact: apply conflict of landing %s in %s kept blocked: compare canonical %s..%s: %v", blocked.Landing, p.ID, short(blocked.Canonical), short(head), err),
			"landing", blocked.Landing, "project", p.ID, "error", err.Error())
		return true
	}
	for _, path := range changed {
		if slices.Contains(blocked.Paths, path) {
			return false
		}
	}
	return true
}

// changedBetween lists the paths that differ between two canonical
// snapshots, read wherever the project's objects are kept.
func (s *Store) changedBetween(ctx context.Context, p project.Project, from, to string) ([]string, error) {
	if metadataOnly(p) {
		return s.changedOnNode(ctx, p, from, to)
	}
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return repo.Changed(ctx, from, to)
}

// ErrNotBlocked refuses to unblock a queued result that is no longer
// stuck on the landing named, or is stuck on a conflict that has a merge
// to work from and is resolved rather than retried.
var ErrNotBlocked = errors.New("the result is not stuck on that landing")

// Unblock clears why a queued result is stuck, so the next pass lands it
// again whatever the canonical is. It is the explicit retry for a conflict
// nothing automatic will clear and no merge can resolve — a result refused
// for writing into a nested repository, once that directory has been dealt
// with. landingID is the stop the caller saw: a result that has since
// stopped again, on another landing, is left for the caller to look at.
func (s *Store) Unblock(ctx context.Context, projectID, artifactID, landingID string) error {
	return s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		raw, err := tx.Bindings(pendingKind)
		if err != nil {
			return err
		}
		id := projectID + "/" + artifactID
		data, ok := raw[id]
		if !ok {
			return fmt.Errorf("%w: artifact %s is not queued for %s", ErrNotBlocked, short(artifactID), projectID)
		}
		var item Pending
		if err := json.Unmarshal(data, &item); err != nil {
			return err
		}
		if item.Blocked == nil || item.Blocked.Landing != landingID {
			return fmt.Errorf("%w: artifact %s, landing %s", ErrNotBlocked, short(artifactID), landingID)
		}
		if item.Blocked.State != LandApplyConflicted {
			return fmt.Errorf("%w: landing %s stopped on a merge conflict, which is resolved rather than retried", ErrNotBlocked, landingID)
		}
		item.Blocked = nil
		return tx.PutBinding(pendingKind, id, item)
	})
}

// markedStillThere says the conflict this entry is blocked on can still be
// worked: the half-merged snapshot has to be a recorded artifact for a
// workspace to be made from it. A block whose snapshot is gone — an older
// record, a repository rebuilt underneath — is not a block worth keeping,
// and merging again is what writes a usable one.
func (s *Store) markedStillThere(ctx context.Context, blocked Blocked) bool {
	if blocked.Marked == "" {
		return true
	}
	_, ok, err := s.Manifest(ctx, blocked.Marked)
	return err != nil || ok
}

// Attempting records that a resolution has been started for a conflict,
// against the canonical it is stuck on. It is what keeps an automatic
// resolution from being started again every sweep while the first one is
// running, and from being retried forever when it did not work.
func (s *Store) Attempting(ctx context.Context, projectID, artifactID, taskID string) error {
	return s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		raw, err := tx.Bindings(pendingKind)
		if err != nil {
			return err
		}
		id := projectID + "/" + artifactID
		var item Pending
		if err := json.Unmarshal(raw[id], &item); err != nil {
			return err
		}
		if item.Blocked == nil {
			return fmt.Errorf("artifact %s is not blocked on a conflict", short(artifactID))
		}
		item.Blocked.Attempt = taskID
		return tx.PutBinding(pendingKind, id, item)
	})
}

// Stuck lists the project's queued results that are held up by a merge
// conflict, oldest first.
func (s *Store) Stuck(ctx context.Context, projectID string) ([]Stuck, error) {
	return s.blocked(ctx, projectID)
}

// AllStuck is every project's blocked result, oldest first. A console
// showing "is anything in conflict anywhere" has to ask once rather than
// once per project, and must not miss a project it did not think to ask
// about.
func (s *Store) AllStuck(ctx context.Context) ([]Stuck, error) {
	return s.blocked(ctx, "")
}

// StuckOne finds one blocked result by artifact id, whichever project it
// belongs to, so an action aimed at a single conflict does not need the
// caller to already know where it lives.
func (s *Store) StuckOne(ctx context.Context, artifactID string) (Stuck, bool, error) {
	all, err := s.blocked(ctx, "")
	if err != nil {
		return Stuck{}, false, err
	}
	for _, item := range all {
		if item.Artifact == artifactID {
			return item, true, nil
		}
	}
	return Stuck{}, false, nil
}

// blocked reads the pending queue for entries stopped on a conflict. An
// empty projectID means every project.
func (s *Store) blocked(ctx context.Context, projectID string) ([]Stuck, error) {
	raw, err := s.ledger.Bindings(ctx, pendingKind)
	if err != nil {
		return nil, err
	}
	var out []Stuck
	for _, data := range raw {
		var item Pending
		if err := json.Unmarshal(data, &item); err != nil || item.Blocked == nil {
			continue
		}
		if projectID != "" && item.Project != projectID {
			continue
		}
		out = append(out, Stuck{
			Project: item.Project, Artifact: item.Artifact, By: item.By,
			Landing: item.Blocked.Landing, State: item.Blocked.State, Canonical: item.Blocked.Canonical,
			Marked: item.Blocked.Marked, Paths: item.Blocked.Paths, At: item.Blocked.At, Attempt: item.Blocked.Attempt,
			Reason: item.Blocked.Reason,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].Artifact < out[j].Artifact
		}
		return out[i].At.Before(out[j].At)
	})
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

// acquireCanonical takes the project's canonical lock for holder from the
// region its canonical workspace is in, "" being the hub's. A region that
// cannot be read is an error: a lock taken from the wrong issuer would
// exclude nobody.
func (s *Store) acquireCanonical(ctx context.Context, p project.Project, holder string) (ledger.Lease, error) {
	region := ""
	if p.Home.Node != "" {
		var err error
		if region, err = s.nodes.Region(ctx, p.Home.Node); err != nil {
			return ledger.Lease{}, fmt.Errorf("region of %s, home of %s: %w", p.Home.Node, p.ID, err)
		}
	}
	return s.ledger.AcquireIn(ctx, region, canonicalLock(p.ID), holder, landTTL)
}
