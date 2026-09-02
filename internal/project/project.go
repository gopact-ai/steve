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
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
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
	DefaultRole Role `json:"default_role,omitempty"`
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
	return p, nil
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
	// KindWorktree is an isolated checkout made for one attempt.
	KindWorktree Kind = "worktree"
)

// Workspace is a directory an attempt can run in, on one node.
type Workspace struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Node    string `json:"node"`
	Path    string `json:"path"`
	Kind    Kind   `json:"kind"`
	// Base is the artifact an isolated workspace was materialised from.
	Base string `json:"base,omitempty"`
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

// NotHomeError carries the two places so the message can name them.
type NotHomeError struct {
	Project  string
	Home     string
	Wanted   string
	Isolated bool
}

func (e NotHomeError) Error() string {
	if e.Isolated {
		return fmt.Sprintf("project %s: isolated workspace on %s is not available yet (home is %s)", e.Project, nodeLabel(e.Wanted), nodeLabel(e.Home))
	}
	return fmt.Sprintf("project %s lives on %s, not %s", e.Project, nodeLabel(e.Home), nodeLabel(e.Wanted))
}

func (e NotHomeError) Is(target error) bool {
	if e.Isolated {
		return target == ErrNotMaterializable
	}
	return target == ErrNotHome
}

func nodeLabel(node string) string {
	if node == "" {
		return "hub"
	}
	return node
}

const (
	kindProject     = "project"
	kindBinding     = "conversation-project"
	bindingNameBase = "conversation/"
)

// Store keeps projects and bindings in the ledger.
type Store struct {
	l   *ledger.Ledger
	now func() time.Time
}

func Open(l *ledger.Ledger) *Store {
	return &Store{l: l, now: time.Now}
}

// Declare records the operator's projects as the hub's assignment. It is
// run at boot from config; the ledger copy is what the runtime reads, so a
// project removed from config stays known until it is retired here.
func (s *Store) Declare(ctx context.Context, projects []Project) error {
	for _, p := range projects {
		normalized, err := p.normalized()
		if err != nil {
			return err
		}
		if err := s.l.PutBinding(ctx, kindProject, normalized.ID, normalized); err != nil {
			return err
		}
	}
	return nil
}

// Get reads one project.
func (s *Store) Get(ctx context.Context, id string) (Project, bool, error) {
	var p Project
	ok, err := s.l.GetBinding(ctx, kindProject, id, &p)
	return p, ok, err
}

// List returns every project, sorted by id.
func (s *Store) List(ctx context.Context) ([]Project, error) {
	raw, err := s.l.Bindings(ctx, kindProject)
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(raw))
	for id := range raw {
		p, ok, err := s.Get(ctx, id)
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

// Materialize serves the canonical workspace on the project's home node.
// Anything else is an honest refusal: the artifact store, not this
// package, knows how to put a project somewhere it is not.
func (s *Store) Materialize(ctx context.Context, req Request) (Workspace, error) {
	p, ok, err := s.Get(ctx, req.Project)
	if err != nil {
		return Workspace{}, err
	}
	if !ok {
		return Workspace{}, fmt.Errorf("%w: %s", ErrUnknown, req.Project)
	}
	if req.Isolated || req.Node != p.Home.Node {
		return Workspace{}, NotHomeError{Project: p.ID, Home: p.Home.Node, Wanted: req.Node, Isolated: req.Isolated}
	}
	return Workspace{
		ID: "canonical:" + p.ID, Project: p.ID, Node: p.Home.Node, Path: p.Home.Path, Kind: KindCanonical,
	}, nil
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

// Grant records a principal's role in a project.
func (s *Store) Grant(ctx context.Context, projectID, principal string, role Role, by string) (Grant, error) {
	if !role.Valid() {
		return Grant{}, fmt.Errorf("role %q is not none, read, write or admin", role)
	}
	if _, ok, err := s.Get(ctx, projectID); err != nil {
		return Grant{}, err
	} else if !ok {
		return Grant{}, fmt.Errorf("%w: %s", ErrUnknown, projectID)
	}
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return Grant{}, errors.New("a grant needs a principal")
	}
	g := Grant{Project: projectID, Principal: principal, Role: role, By: by, At: s.now().UTC()}
	return g, s.l.PutBinding(ctx, kindGrant, projectID+"/"+principal, g)
}

// Grants lists a project's grants.
func (s *Store) Grants(ctx context.Context, projectID string) ([]Grant, error) {
	raw, err := s.l.Bindings(ctx, kindGrant)
	if err != nil {
		return nil, err
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
	DisclosureProposed = "proposed"
	DisclosureApproved = "approved"
	DisclosureDenied   = "denied"
	kindDisclosureOp   = "disclosure-request"
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
