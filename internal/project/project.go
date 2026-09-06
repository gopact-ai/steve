// Package project is the logical unit of work Steve acts on and the binding
// that says where its files live.
//
// A Project is a name with hub-assigned facts: its level, whether agents
// edit it in place or in isolation, its pinned skills, and the places its
// artifacts count as durable. Its ProjectHome is a binding to one (node,
// path): the canonical workspace, which sits outside Steve's authority and
// is what an in-place chat turn edits directly. A conversation points at a
// project through a versioned binding; a task fixes the project it was
// created under and never changes it.
//
// Every question of the form "which directory does this attempt run in"
// goes through Materialize. In this version it answers only for the
// canonical workspace on the project's home node; isolated worktrees and
// materialisation on other nodes arrive with the artifact store, behind
// the same call.
package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Level is the data level the hub assigns a project. Levels order
// public < internal < restricted < sealed; an artifact's label is the
// maximum over its closure and a node may only hold what its own level
// admits.
type Level string

const (
	LevelPublic     Level = "public"
	LevelInternal   Level = "internal"
	LevelRestricted Level = "restricted"
	LevelSealed     Level = "sealed"
)

var levelOrder = map[Level]int{LevelPublic: 0, LevelInternal: 1, LevelRestricted: 2, LevelSealed: 3}

// Admits reports whether data at level l may sit at a place of level at.
func (l Level) Admits(at Level) bool { return levelOrder[at] >= levelOrder[l] }

// Valid says whether the level is one of the four.
func (l Level) Valid() bool { _, ok := levelOrder[l]; return ok }

// OrDefault is internal when unset.
func (l Level) OrDefault() Level {
	if l == "" {
		return LevelInternal
	}
	return l
}

// RepoMode is the user's explicit choice of how agents touch the project.
type RepoMode string

const (
	// RepoInPlace lets an interactive turn edit the canonical workspace
	// directly under the canonical write lock. Steve promises no atomic
	// landing for it; that is what the user chose.
	RepoInPlace RepoMode = "inplace"
	// RepoIsolated runs every turn in a worktree and lands results through
	// the landing protocol.
	RepoIsolated RepoMode = "isolated"
)

// Home is the ProjectHome binding: the node and path of the canonical
// workspace. Node "" is the hub.
type Home struct {
	Node string `json:"node"`
	Path string `json:"path"`
}

// Project is the logical record plus its home binding.
type Project struct {
	ID    string   `json:"id"`
	Level Level    `json:"level"`
	Repo  RepoMode `json:"repo"`
	// Skills are the skill sources pinned for this project.
	Skills []string `json:"skills,omitempty"`
	// DurablePlaces are the nodes an artifact must reach to count as
	// durable. Empty means the hub. A sealed project's home node is one of
	// them by construction: its data never leaves.
	DurablePlaces  []string `json:"durable_places,omitempty"`
	ExternalRemote string   `json:"external_remote,omitempty"`
	Home           Home     `json:"home"`
	// DefaultRole is what a principal without a grant gets. Empty means
	// write for public and internal projects, none for restricted and
	// sealed ones.
	DefaultRole  Role            `json:"default_role,omitempty"`
	ConfigGrants map[string]Role `json:"config_grants,omitempty"`
	// Copies are the project's workspaces away from home, by machine.
	Copies map[string]Copy `json:"copies,omitempty"`
}

// Durable reports whether node is one of the places this project's
// artifacts are durable at.
func (p Project) Durable(node string) bool {
	if len(p.DurablePlaces) == 0 {
		return node == ""
	}
	for _, place := range p.DurablePlaces {
		if place == node {
			return true
		}
	}
	return false
}

func (p Project) normalized() (Project, error) {
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		return p, errors.New("project id is required")
	}
	if strings.ContainsAny(p.ID, "/ \t") {
		return p, fmt.Errorf("project id %q may not contain slashes or spaces", p.ID)
	}
	if p.Level == "" {
		p.Level = LevelInternal
	}
	if _, ok := levelOrder[p.Level]; !ok {
		return p, fmt.Errorf("project %s: level %q is not public, internal, restricted or sealed", p.ID, p.Level)
	}
	switch p.Repo {
	case "":
		p.Repo = RepoInPlace
	case RepoInPlace, RepoIsolated:
	default:
		return p, fmt.Errorf("project %s: repo %q is not inplace or isolated", p.ID, p.Repo)
	}
	if p.Home.Path == "" {
		return p, fmt.Errorf("project %s: home.path is required", p.ID)
	}
	if p.Level == LevelSealed && !p.Durable(p.Home.Node) {
		p.DurablePlaces = append(p.DurablePlaces, p.Home.Node)
	}
	if p.DefaultRole != "" && !p.DefaultRole.Valid() {
		return p, fmt.Errorf("project %s: default_role %q is not none, read, write or admin", p.ID, p.DefaultRole)
	}
	grants := make(map[string]Role, len(p.ConfigGrants))
	for principal, role := range p.ConfigGrants {
		g, err := normalizeGrant(p.ID, principal, role, "config", time.Time{})
		if err != nil {
			return p, err
		}
		if _, exists := grants[g.Principal]; exists {
			return p, fmt.Errorf("duplicate configured grant for %s", g.Principal)
		}
		grants[g.Principal] = g.Role
	}
	if len(grants) > 0 {
		p.ConfigGrants = grants
	} else {
		p.ConfigGrants = nil
	}
	for node, c := range p.Copies {
		fixed, err := p.copyShape(node, c)
		if err != nil {
			return p, err
		}
		p.Copies[node] = fixed
	}
	return p, nil
}

// copyShape is what every copy must satisfy, declared or added: not on
// the home machine, an absolute directory, a state, and — for a sealed
// project — not at all.
func (p Project) copyShape(node string, c Copy) (Copy, error) {
	if p.Level == LevelSealed {
		return c, fmt.Errorf("project %s is sealed: its data stays at %s, so it cannot have copies", p.ID, nodeLabel(p.Home.Node))
	}
	if node == p.Home.Node {
		return c, fmt.Errorf("project %s already lives on %s: that is its home, not a copy", p.ID, nodeLabel(node))
	}
	c.Node = node
	c.Path = strings.TrimSpace(c.Path)
	if c.Path == "" || !strings.HasPrefix(c.Path, "/") {
		return c, fmt.Errorf("project %s: copy on %s: path %q must be absolute", p.ID, nodeLabel(node), c.Path)
	}
	c.Path = filepath.Clean(c.Path)
	if c.Origin == "" {
		c.Origin = OriginAdopted
	}
	if c.State == "" {
		c.State = CopyReady
	}
	return c, nil
}

// Binding is a conversation's current project, with the version that lets
// a session say exactly which binding it was opened under.
type Binding struct {
	ConversationID string    `json:"conversation_id"`
	ProjectID      string    `json:"project_id"`
	Version        int64     `json:"version"`
	By             string    `json:"by"`
	At             time.Time `json:"at"`
}

// Kind says what a materialised workspace is.
type Kind string

const (
	// KindCanonical is the user's directory itself: the ProjectHome path.
	KindCanonical Kind = "canonical"
	// KindCopy is a long-lived directory of the project on a machine other
	// than its home, declared by the user: adopted where it already was,
	// or cloned. Interactive turns run there when the agent is on that
	// machine. It is not landed; it keeps up with the canonical workspace
	// through git, which is what the user chose by declaring it.
	KindCopy Kind = "copy"
	// KindWorktree is an isolated checkout made for one attempt.
	KindWorktree Kind = "worktree"
)

// Origin says how a copy came to be.
type Origin string

const (
	// OriginAdopted is a directory that already existed on the machine.
	OriginAdopted Origin = "adopted"
	// OriginCloned is a directory Steve made from the project's remote or
	// from a snapshot of its canonical workspace.
	OriginCloned Origin = "cloned"
)

// Workspace is a directory an attempt can run in, on one node: the
// resolved place, as an attempt records it. What a copy is and how it
// came to be lives on the project (Copy); a worktree's base is here.
type Workspace struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Node    string `json:"node"`
	Path    string `json:"path"`
	Kind    Kind   `json:"kind"`
	// Base is the artifact an isolated workspace was materialised from.
	Base string `json:"base,omitempty"`
}

// CopyState is where a copy is in its life: being cloned, usable, or
// failed to come up.
type CopyState string

const (
	CopyReady        CopyState = "ready"
	CopyProvisioning CopyState = "provisioning"
	CopyFailed       CopyState = "failed"
)

// Copy is a project's long-lived directory on a machine other than its
// home, declared by the user: adopted where it already was, or cloned.
// Interactive turns run there when the agent is on that machine. It is
// not landed; it keeps up with the canonical workspace through git, which
// is what the user chose by declaring it. Copies live on the project
// record, so a project and where it is change together.
type Copy struct {
	Node   string    `json:"node"`
	Path   string    `json:"path"`
	Origin Origin    `json:"origin"`
	Source string    `json:"source,omitempty"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at,omitzero"`
	State  CopyState `json:"state"`
	Error  string    `json:"error,omitempty"`
}

// Canonical is the project's home as a workspace: the directory itself.
func (p Project) Canonical() Workspace {
	return Workspace{ID: "canonical:" + p.ID, Project: p.ID, Node: p.Home.Node, Path: p.Home.Path, Kind: KindCanonical}
}

// CopyID names a project's copy on a machine. A project id may not
// contain a slash, so the pair reads back unambiguously; the hub is "".
func CopyID(projectID, node string) string { return "copy:" + projectID + "/" + node }

// CopyOn is the project's copy on a machine, if it has one.
func (p Project) CopyOn(node string) (Copy, bool) {
	c, ok := p.Copies[node]
	return c, ok
}

// Workspaces is where the project is: its home first, then its copies
// by machine, every state included so a page can show one coming up.
func (p Project) Workspaces() []Workspace {
	out := []Workspace{p.Canonical()}
	nodes := make([]string, 0, len(p.Copies))
	for node := range p.Copies {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		out = append(out, p.Copies[node].workspace(p.ID, node))
	}
	return out
}

func (c Copy) workspace(projectID, node string) Workspace {
	return Workspace{ID: CopyID(projectID, node), Project: projectID, Node: node, Path: c.Path, Kind: KindCopy}
}

// Place is the one rule for where a project may be worked in place: on
// its home machine, in the canonical workspace; on a machine that holds
// a ready copy, in that copy; nowhere else. Every surface that asks "can
// this agent work here" — a turn, the console's context bar, the read
// model's projection — asks this, so the answer is the same everywhere.
// The refusal names where the project is.
func (p Project) Place(node string) (Workspace, error) {
	if node == p.Home.Node {
		return p.Canonical(), nil
	}
	if c, ok := p.Copies[node]; ok && c.State == CopyReady {
		return c.workspace(p.ID, node), nil
	}
	return Workspace{}, NotHomeError{Project: p.ID, Home: p.Home.Node, Wanted: node, Places: p.Workspaces()}
}

// Request asks for a workspace of a project on a node.
type Request struct {
	Project string
	Node    string
	// Isolated asks for a worktree rather than the canonical directory.
	// Plan steps always set it; an in-place chat turn never does.
	Isolated bool
	// Base names the artifact an isolated workspace starts from. Empty is
	// the project's current canonical state.
	Base string
	// Inputs are other artifacts materialised read-only under inputs/<name>
	// inside an isolated workspace: what a merge step converges.
	Inputs []Input
	// Owner is the attempt the workspace is for; it names the directory.
	Owner string
}

// Input is one named artifact a workspace is given alongside its base.
type Input struct {
	Name     string
	Artifact string
}

// Workspaces is the one seam every executor asks for a directory through.
type Workspaces interface {
	Materialize(ctx context.Context, req Request) (Workspace, error)
}

// Errors callers turn into user-facing text.
var (
	ErrUnknown = errors.New("project: unknown project")
	// ErrNotHome is a canonical-workspace request on a node other than
	// the project's home: the directory does not exist there.
	ErrNotHome = errors.New("project: not the project's home node")
	// ErrNotMaterializable is a request this version cannot serve yet —
	// an isolated worktree, or a workspace away from home — which the
	// artifact store will.
	ErrNotMaterializable = errors.New("project: workspace cannot be materialised here")
)

// NotHomeError says a project has no workspace on the machine asked for,
// and carries where it does have them so the message can name places.
type NotHomeError struct {
	Project  string
	Home     string
	Wanted   string
	Isolated bool
	// Places are the machines the project has a workspace on: its home
	// first, then its copies.
	Places []Workspace
}

func (e NotHomeError) Error() string {
	if e.Isolated {
		return fmt.Sprintf("project %s: isolated workspace on %s is not available yet (home is %s)", e.Project, nodeLabel(e.Wanted), nodeLabel(e.Home))
	}
	return fmt.Sprintf("project %s lives on %s, not %s", e.Project, e.PlaceList(), nodeLabel(e.Wanted))
}

// PlaceList names the machines the project has a workspace on, home
// first: "hub (home), node-a".
func (e NotHomeError) PlaceList() string {
	if len(e.Places) == 0 {
		return nodeLabel(e.Home)
	}
	parts := make([]string, 0, len(e.Places))
	for _, ws := range e.Places {
		if ws.Kind == KindCanonical {
			parts = append(parts, nodeLabel(ws.Node)+" (home)")
		} else {
			parts = append(parts, nodeLabel(ws.Node))
		}
	}
	return strings.Join(parts, ", ")
}

func (e NotHomeError) Is(target error) bool {
	if e.Isolated {
		return target == ErrNotMaterializable
	}
	return target == ErrNotHome
}

func nodeLabel(node string) string { return nodewire.Place(node) }

const (
	kindProject     = "project"
	kindBinding     = "conversation-project"
	bindingNameBase = "conversation/"
)

// Store keeps projects and bindings in the ledger.
type Store struct {
	l           *ledger.Ledger
	now         func() time.Time
	declaration atomic.Pointer[string]
	hubID       atomic.Pointer[string]
	guards      []DeclarationGuard
	// Levels answers a machine's data level, when the store is given a way
	// to know; a copy may only sit where the project's level admits.
	Levels func(node string) Level
}

// admits checks a copy's machine against the project's level.
func (s *Store) admits(p Project, node string) error {
	if s.Levels == nil {
		return nil
	}
	at := s.Levels(node)
	if !p.Level.OrDefault().Admits(at.OrDefault()) {
		return fmt.Errorf("project %s is %s; %s is only %s", p.ID, p.Level.OrDefault(), nodeLabel(node), at.OrDefault())
	}
	return nil
}

type DeclarationGuard func(*ledger.Tx, []Project) error

func Open(l *ledger.Ledger, guards ...DeclarationGuard) *Store {
	return &Store{l: l, now: time.Now, guards: append([]DeclarationGuard(nil), guards...)}
}

func (s *Store) guardDeclaration(tx *ledger.Tx, desired map[string]Project) error {
	if err := s.guardOwners(tx, desired); err != nil {
		return err
	}
	if err := validateCloneOwnership(tx, desired); err != nil {
		return err
	}
	values := make([]Project, 0, len(desired))
	for _, p := range desired {
		values = append(values, p)
	}
	for _, guard := range s.guards {
		if err := guard(tx, values); err != nil {
			return err
		}
	}
	return nil
}

// Declare records the operator's projects as the hub's assignment. It is
// run at boot from config; the ledger copy is what the runtime reads, so a
// project removed from config stays known until it is retired here.
//
// Config declares where a project's copies are; the ledger remembers how
// each came to be. A copy declared at the same place keeps its record —
// origin, source, who added it, whether it is ready; one declared at a
// new place starts over as adopted; one no longer declared is forgotten.
func (s *Store) Declare(ctx context.Context, projects []Project) error {
	declared := make([]Project, 0, len(projects))
	seen := map[string]bool{}
	for _, p := range projects {
		p.Copies = maps.Clone(p.Copies)
		normalized, err := p.normalized()
		if err != nil {
			return err
		}
		if seen[normalized.ID] {
			return fmt.Errorf("project %s is declared more than once", normalized.ID)
		}
		seen[normalized.ID] = true
		for node := range normalized.Copies {
			if err := s.admits(normalized, node); err != nil {
				return err
			}
		}
		declared = append(declared, normalized)
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		all, err := projectsIn(tx)
		if err != nil {
			return err
		}
		for _, p := range declared {
			previous := all[p.ID]
			for node, c := range p.Copies {
				if had, ok := previous.Copies[node]; ok && had.Path == c.Path {
					p.Copies[node] = had
				}
			}
			all[p.ID] = p
		}
		if err := validateOwnership(all); err != nil {
			return err
		}
		if err := s.guardDeclaration(tx, all); err != nil {
			return err
		}
		for _, p := range declared {
			if err := tx.PutBinding(kindProject, p.ID, all[p.ID]); err != nil {
				return err
			}
		}
		return nil
	})
}

func projectsIn(tx *ledger.Tx) (map[string]Project, error) {
	raw, err := tx.Bindings(kindProject)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Project, len(raw))
	for id, data := range raw {
		var p Project
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("read project %s: %w", id, err)
		}
		out[id] = p
	}
	return out, nil
}

// validateOwnership examines the final set inside the same transaction that
// writes it, including homes and copies on every node. Batch replacements do
// not conflict with their own previous locations.
func validateOwnership(projects map[string]Project) error {
	ids := make([]string, 0, len(projects))
	for id := range projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var owned []Workspace
	for _, id := range ids {
		for _, ws := range projects[id].Workspaces() {
			for _, other := range owned {
				if ws.Node == other.Node && pathsOverlap(ws.Path, other.Path) {
					return fmt.Errorf("project %s workspace %s on %s overlaps project %s workspace %s", ws.Project, ws.Path, nodeLabel(ws.Node), other.Project, other.Path)
				}
			}
			owned = append(owned, ws)
		}
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}

// Retire forgets a project the operator no longer wants. Conversations
// bound to it keep their binding until they switch; tasks created under
// it keep their record. Nothing on disk is touched.
func (s *Store) Retire(ctx context.Context, id string) error {
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		all, err := projectsIn(tx)
		if err != nil {
			return err
		}
		delete(all, id)
		if err := s.guardDeclaration(tx, all); err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM bindings WHERE kind = ? AND id = ?", kindProject, id)
		return err
	})
}

// Get reads one project.
func (s *Store) Get(ctx context.Context, id string) (Project, bool, error) {
	if err := s.checkOwner(ctx, id); err != nil {
		return Project{}, false, err
	}
	if err := s.checkDeclaration(ctx); err != nil {
		return Project{}, false, err
	}
	var p Project
	ok, err := s.l.GetBinding(ctx, kindProject, id, &p)
	return p, ok, err
}

// List returns every project, sorted by id.
func (s *Store) List(ctx context.Context) ([]Project, error) {
	if err := s.checkDeclaration(ctx); err != nil {
		return nil, err
	}
	raw, err := s.l.Bindings(ctx, kindProject)
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(raw))
	for id := range raw {
		p, ok, err := s.Lookup(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Binding reads a conversation's current project binding.
func (s *Store) Binding(ctx context.Context, conversationID string) (Binding, bool, error) {
	var b Binding
	ok, err := s.l.GetBinding(ctx, kindBinding, conversationID, &b)
	return b, ok, err
}

// Bind points a conversation at a project. The version moves forward under
// a compare-and-set on the binding's name, so two concurrent switches
// cannot both believe they won.
func (s *Store) Bind(ctx context.Context, conversationID, projectID, by string) (Binding, error) {
	if _, ok, err := s.Get(ctx, projectID); err != nil {
		return Binding{}, err
	} else if !ok {
		return Binding{}, fmt.Errorf("%w: %s", ErrUnknown, projectID)
	}
	current, _, err := s.l.Name(ctx, bindingNameBase+conversationID+"/project")
	if err != nil {
		return Binding{}, err
	}
	b := Binding{ConversationID: conversationID, ProjectID: projectID, By: by, At: s.now().UTC()}
	err = s.l.Update(ctx, func(tx *ledger.Tx) error {
		version, err := tx.CompareAndSetName(bindingNameBase+conversationID+"/project", current.Version, projectID)
		if err != nil {
			return err
		}
		b.Version = version
		return tx.PutBinding(kindBinding, conversationID, b)
	})
	if err != nil {
		return Binding{}, err
	}
	return b, nil
}

// Conflict finds the workspace, of any project, whose directory on node is
// the same as path or nests with it. A directory belongs to one workspace:
// two would both claim the same files, and a single-writer lock on one
// would not know about the other.
func (s *Store) Conflict(ctx context.Context, node, path string) (Workspace, bool, error) {
	list, err := s.List(ctx)
	if err != nil {
		return Workspace{}, false, err
	}
	for _, p := range list {
		for _, ws := range p.Workspaces() {
			if ws.Node != node {
				continue
			}
			if pathsOverlap(ws.Path, path) {
				return ws, true, nil
			}
		}
	}
	return Workspace{}, false, nil
}

// SetCopy records a copy of a project on a machine — a new one, or a
// change of state for one being cloned. A new copy must satisfy the copy
// shape, the machine's level, and own its directory alone; a project has
// one copy per machine.
func (s *Store) SetCopy(ctx context.Context, projectID string, c Copy) (Workspace, error) {
	if err := s.checkDeclaration(ctx); err != nil {
		return Workspace{}, err
	}
	var workspace Workspace
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		all, err := projectsIn(tx)
		if err != nil {
			return err
		}
		p, ok := all[projectID]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknown, projectID)
		}
		c, err = p.copyShape(c.Node, c)
		if err != nil {
			return err
		}
		if had, exists := p.Copies[c.Node]; exists && had.Path != c.Path {
			return fmt.Errorf("project %s already has a copy on %s (%s); a project has one copy per machine", p.ID, nodeLabel(c.Node), had.Path)
		} else if !exists {
			if err := s.admits(p, c.Node); err != nil {
				return err
			}
			if c.At.IsZero() {
				c.At = s.now().UTC()
			}
		}
		if p.Copies == nil {
			p.Copies = map[string]Copy{}
		}
		p.Copies[c.Node] = c
		all[p.ID] = p
		if err := validateOwnership(all); err != nil {
			return err
		}
		if err := s.guardDeclaration(tx, all); err != nil {
			return err
		}
		if err := tx.PutBinding(kindProject, p.ID, p); err != nil {
			return err
		}
		workspace = c.workspace(p.ID, c.Node)
		return nil
	})
	if err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

// DeleteCopy forgets a project's copy on a machine. The directory is not
// touched. Whether something is running there is the caller's check.
func (s *Store) DeleteCopy(ctx context.Context, projectID, node string) error {
	if err := s.checkDeclaration(ctx); err != nil {
		return err
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		all, err := projectsIn(tx)
		if err != nil {
			return err
		}
		p, ok := all[projectID]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknown, projectID)
		}
		if _, has := p.Copies[node]; !has {
			return fmt.Errorf("project %s has no copy on %s", projectID, nodeLabel(node))
		}
		delete(p.Copies, node)
		all[p.ID] = p
		if err := s.guardDeclaration(tx, all); err != nil {
			return err
		}
		return tx.PutBinding(kindProject, p.ID, p)
	})
}

// Materialize serves the project's workspace on a machine for an in-place
// turn: the canonical one at home, a copy elsewhere. An isolated
// workspace is an honest refusal: the artifact store, not this package,
// knows how to put a project somewhere it is not.
func (s *Store) Materialize(ctx context.Context, req Request) (Workspace, error) {
	p, ok, err := s.Get(ctx, req.Project)
	if err != nil {
		return Workspace{}, err
	}
	if !ok {
		return Workspace{}, fmt.Errorf("%w: %s", ErrUnknown, req.Project)
	}
	if req.Isolated {
		return Workspace{}, NotHomeError{Project: p.ID, Home: p.Home.Node, Wanted: req.Node, Isolated: true}
	}
	return p.Place(req.Node)
}

const kindDisclosure = "disclosure"

// Disclose records that project content left Steve through a channel.
func (s *Store) Disclose(ctx context.Context, id string, record any) error {
	return s.l.PutBinding(ctx, kindDisclosure, id, record)
}

// Disclosures lists every disclosure record as raw JSON keyed by id.
func (s *Store) Disclosures(ctx context.Context) (map[string]json.RawMessage, error) {
	return s.l.Bindings(ctx, kindDisclosure)
}

// Role is what a principal may do in a project.
type Role string

const (
	RoleNone  Role = "none"
	RoleRead  Role = "read"
	RoleWrite Role = "write"
	RoleAdmin Role = "admin"
)

var roleOrder = map[Role]int{RoleNone: 0, RoleRead: 1, RoleWrite: 2, RoleAdmin: 3}

// Valid says whether the role is one of the four.
func (r Role) Valid() bool { _, ok := roleOrder[r]; return ok }

// AtLeast compares roles.
func (r Role) AtLeast(want Role) bool { return roleOrder[r] >= roleOrder[want] }

// Grant is a principal's role in a project, and who gave it.
type Grant struct {
	Project   string    `json:"project"`
	Principal string    `json:"principal"`
	Role      Role      `json:"role"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
}

const kindGrant = "grant"
const kindConfigGrant = "config-grant"

func normalizeGrant(projectID, principal string, role Role, by string, at time.Time) (Grant, error) {
	if !role.Valid() {
		return Grant{}, fmt.Errorf("role %q is not none, read, write or admin", role)
	}
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return Grant{}, errors.New("a grant needs a principal")
	}
	return Grant{Project: projectID, Principal: principal, Role: role, By: by, At: at}, nil
}

// Grant records a principal's role in a project.
func (s *Store) Grant(ctx context.Context, projectID, principal string, role Role, by string) (Grant, error) {
	g, err := normalizeGrant(projectID, principal, role, by, s.now().UTC())
	if err != nil {
		return Grant{}, err
	}
	if _, ok, err := s.Get(ctx, projectID); err != nil {
		return Grant{}, err
	} else if !ok {
		return Grant{}, fmt.Errorf("%w: %s", ErrUnknown, projectID)
	}
	kind := kindGrant
	if by == "config" {
		kind = kindConfigGrant
	}
	return g, s.l.PutBinding(ctx, kind, projectID+"/"+g.Principal, g)
}

// Grants lists a project's grants.
func (s *Store) Grants(ctx context.Context, projectID string) ([]Grant, error) {
	raw, err := s.l.Bindings(ctx, kindGrant)
	if err != nil {
		return nil, err
	}
	configured, err := s.l.Bindings(ctx, kindConfigGrant)
	if err != nil {
		return nil, err
	}
	for id, data := range configured {
		raw[id] = data
	}
	var out []Grant
	for _, data := range raw {
		var g Grant
		if err := json.Unmarshal(data, &g); err == nil && (projectID == "" || g.Project == projectID) {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Principal < out[j].Principal })
	return out, nil
}

// Access is the role a principal has in a project: the owner is admin
// everywhere; an explicit grant wins; otherwise the project's default —
// write for public and internal projects, nothing for restricted and
// sealed ones, unless the project says otherwise.
func (s *Store) Access(ctx context.Context, projectID, principal, owner string) (Role, error) {
	p, ok, err := s.Get(ctx, projectID)
	if err != nil {
		return RoleNone, err
	}
	if !ok {
		return RoleNone, fmt.Errorf("%w: %s", ErrUnknown, projectID)
	}
	if owner != "" && principal == owner {
		return RoleAdmin, nil
	}
	if principal != "" {
		var configured Grant
		if ok, err := s.l.GetBinding(ctx, kindConfigGrant, projectID+"/"+principal, &configured); err != nil {
			return RoleNone, err
		} else if ok {
			return configured.Role, nil
		}
		var g Grant
		if ok, err := s.l.GetBinding(ctx, kindGrant, projectID+"/"+principal, &g); err != nil {
			return RoleNone, err
		} else if ok {
			return g.Role, nil
		}
	}
	if p.DefaultRole != "" {
		return p.DefaultRole, nil
	}
	switch p.Level.OrDefault() {
	case LevelRestricted, LevelSealed:
		return RoleNone, nil
	}
	return RoleWrite, nil
}

// DisclosureRequest is the metadata of content waiting to leave a sealed
// project. The content itself never enters the ledger: the hub is not a
// place sealed data lives.
type DisclosureRequest struct {
	ID             string    `json:"id"`
	Project        string    `json:"project"`
	TaskID         string    `json:"task_id,omitempty"`
	Attempt        string    `json:"attempt"`
	ConversationID string    `json:"conversation_id"`
	Requester      string    `json:"requester"`
	Bytes          int       `json:"bytes"`
	ProposedAt     time.Time `json:"proposed_at"`
	ResolvedBy     string    `json:"resolved_by,omitempty"`
}

const (
	DisclosureProposed    = "proposed"
	DisclosureApproved    = "approved"
	DisclosureDenied      = "denied"
	DisclosureInterrupted = "interrupted"
	kindDisclosureOp      = "disclosure-request"
)

// ProposeDisclosure opens the operation; the content waits elsewhere.
func (s *Store) ProposeDisclosure(ctx context.Context, req DisclosureRequest) error {
	req.ProposedAt = s.now().UTC()
	_, err := s.l.Begin(ctx, req.ID, kindDisclosureOp, DisclosureProposed, req.Requester, req)
	return err
}

// ResolveDisclosure is the owner's decision.
func (s *Store) ResolveDisclosure(ctx context.Context, id string, approved bool, by string) error {
	to := DisclosureDenied
	if approved {
		to = DisclosureApproved
	}
	_, err := s.l.Transition(ctx, id, DisclosureProposed, to, by, nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		var req DisclosureRequest
		if err := json.Unmarshal(op.Data, &req); err != nil {
			return err
		}
		req.ResolvedBy = by
		return tx.SetData(op, req)
	})
	return err
}

// PendingDisclosures lists disclosure requests awaiting the owner.
func (s *Store) PendingDisclosures(ctx context.Context) ([]DisclosureRequest, error) {
	ops, err := s.l.Operations(ctx, kindDisclosureOp, DisclosureProposed)
	if err != nil {
		return nil, err
	}
	out := make([]DisclosureRequest, 0, len(ops))
	for _, op := range ops {
		var req DisclosureRequest
		if err := json.Unmarshal(op.Data, &req); err == nil {
			out = append(out, req)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProposedAt.Before(out[j].ProposedAt) })
	return out, nil
}
