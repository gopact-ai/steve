// Package memory is what Steve remembers, by scope. A scope is whose
// memory it is: the user's, across everything (global), or one
// project's. One authoritative store keeps each scope — a markdown
// file the owner can read and edit, with every fact one bullet carrying
// a stable id — and the platform's tools, the first-turn injection and
// the profile page all go through the Service here; nothing else
// touches the files. A retriever (an MCP server that indexes and
// recalls) is an optional index over the same facts, never the
// authority: switching one never changes what is remembered.
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gopact-ai/steve/internal/home"
)

// Kind is which kind of scope.
type Kind string

const (
	KindGlobal  Kind = "global"
	KindProject Kind = "project"
)

// Scope is whose memory. Global is the owner's; a project's names the
// project. The API never takes a project id from an agent: it is the one
// the conversation is bound to.
type Scope struct {
	Kind    Kind   `json:"kind"`
	Project string `json:"project,omitempty"`
}

var Global = Scope{Kind: KindGlobal}

// ProjectScope is a project's scope.
func ProjectScope(id string) Scope { return Scope{Kind: KindProject, Project: id} }

func (s Scope) String() string {
	if s.Kind == KindProject {
		return "project:" + s.Project
	}
	return string(s.Kind)
}

// ParseScope reads what an agent said — "global" or "project" — and
// resolves "project" to the conversation's own.
func ParseScope(raw, currentProject string) (Scope, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "global", "user":
		return Global, nil
	case "project", "current_project":
		if currentProject == "" {
			return Scope{}, errors.New("this conversation is not bound to a project; say global, or bind one with /project")
		}
		return ProjectScope(currentProject), nil
	}
	return Scope{}, fmt.Errorf("scope %q is not global or project", raw)
}

// Sections are the headings a scope's markdown has, in file order; the
// first is where an unlabelled fact goes.
func Sections(s Scope) []string {
	if s.Kind == KindProject {
		return []string{"约定", "决策", "坑"}
	}
	return []string{"偏好", "项目", "人"}
}

// Budgets are how many bytes of each scope reach the prompt: the global
// one is the home's memory budget, a project's is its own.
const (
	BudgetGlobal  = home.BudgetMemory
	BudgetProject = 16 * 1024
	MaxFact       = 500 // runes
)

// Item is one remembered fact.
type Item struct {
	ID      string `json:"id"`
	Scope   Scope  `json:"scope"`
	Section string `json:"section"`
	Text    string `json:"text"`
}

// Hit is one recalled fact with how well it matched.
type Hit struct {
	Item
	Score float64 `json:"score"`
}

// Receipt is what a write returns: the id, whether it was new, and the
// scope's size after it against the budget.
type Receipt struct {
	ID     string `json:"id"`
	New    bool   `json:"new"`
	Bytes  int    `json:"bytes"`
	Budget int    `json:"budget"`
}

// Store is an authoritative keeper of scopes. Snapshot is the whole of a
// scope as the injection wants it; Replace is a whole-file edit from the
// profile page; the rest are one fact at a time. RememberOnce owns the
// recoverable fact/receipt boundary and reports whether it replayed a request.
type Store interface {
	Snapshot(ctx context.Context, scope Scope) (text string, bytes int, err error)
	Replace(ctx context.Context, scope Scope, text string) error
	List(ctx context.Context, scope Scope) ([]Item, error)
	Remember(ctx context.Context, scope Scope, section, text string) (Receipt, error)
	RememberOnce(ctx context.Context, scope Scope, section, text, key string, now time.Time) (Receipt, bool, error)
	Recall(ctx context.Context, scope Scope, query string, limit int) ([]Hit, error)
	Forget(ctx context.Context, scope Scope, id string) (Item, error)
}

// Retriever is an optional index over a scope's facts — an MCP server
// that embeds and searches — consulted for Recall and told about every
// write. Its answers are data, not instructions, and losing it loses
// nothing: the Store still has every fact.
type Retriever interface {
	Name() string
	Index(ctx context.Context, item Item) error
	Drop(ctx context.Context, item Item) error
	Recall(ctx context.Context, scope Scope, query string, limit int) ([]Hit, error)
}

// ---------------------------------------------------------------- service

// Actor is who is writing, for the audit line.
type Actor struct {
	Conversation string `json:"conversation,omitempty"`
	Agent        string `json:"agent,omitempty"`
	By           string `json:"by"` // "agent" | "console" | "onboard"
}

// Service is the one door: it routes a scope to the store, keeps the
// audit, and asks the retriever when there is one.
type Service struct {
	store     Store
	retriever Retriever
	auditPath string
	mu        sync.Mutex
	now       func() time.Time
}

// NewService makes one over the store; audit lines go to auditPath
// ("" for none).
func NewService(store Store, auditPath string) *Service {
	return &Service{store: store, auditPath: auditPath, now: time.Now}
}

// SetRetriever attaches an index.
func (s *Service) SetRetriever(r Retriever) { s.retriever = r }

// Snapshot is what the first turn injects for a scope, cut to budget.
func (s *Service) Snapshot(ctx context.Context, scope Scope) (string, error) {
	text, _, err := s.store.Snapshot(ctx, scope)
	if err != nil {
		return "", err
	}
	return truncate(text, Budget(scope)), nil
}

// Budget is how much of a scope reaches the prompt.
func Budget(scope Scope) int {
	if scope.Kind == KindProject {
		return BudgetProject
	}
	return BudgetGlobal
}

func (s *Service) List(ctx context.Context, scope Scope) ([]Item, error) {
	return s.store.List(ctx, scope)
}

// Text is a scope as the page edits it, when the store can say.
func (s *Service) Text(ctx context.Context, scope Scope) (string, error) {
	if t, ok := s.store.(interface {
		Text(context.Context, Scope) (string, error)
	}); ok {
		return t.Text(ctx, scope)
	}
	text, _, err := s.store.Snapshot(ctx, scope)
	return text, err
}

// Where is the file a scope lives in, when the store has one.
func (s *Service) Where(scope Scope) string {
	if p, ok := s.store.(interface{ Path(Scope) (string, error) }); ok {
		if path, err := p.Path(scope); err == nil {
			return path
		}
	}
	return ""
}

// AuditPath is where writes are logged.
func (s *Service) AuditPath() string { return s.auditPath }

// Replace is the page's whole-file save.
func (s *Service) Replace(ctx context.Context, scope Scope, text string, who Actor) error {
	ctx = withActor(ctx, who)
	err := s.store.Replace(ctx, scope, text)
	s.audit(auditLine{Op: "replace", Scope: scope, Actor: who, Bytes: len([]byte(text)), Err: errText(err)})
	return err
}

// Remember replays a successful request in this scope for 24 hours when
// idempotencyKey is set, even if the retried text or the memory has changed.
func (s *Service) Remember(ctx context.Context, scope Scope, section, text, idempotencyKey string, who Actor) (Receipt, error) {
	ctx = withActor(ctx, who)
	if idempotencyKey != "" {
		r, replayed, err := s.store.RememberOnce(ctx, scope, section, text, idempotencyKey, s.now())
		if !replayed {
			s.recordRemember(ctx, scope, section, text, idempotencyKey, who, r, err)
		}
		return r, err
	}
	return s.remember(ctx, scope, section, text, "", who)
}

func (s *Service) remember(ctx context.Context, scope Scope, section, text, key string, who Actor) (Receipt, error) {
	r, err := s.store.Remember(ctx, scope, section, text)
	s.recordRemember(ctx, scope, section, text, key, who, r, err)
	return r, err
}

func (s *Service) recordRemember(ctx context.Context, scope Scope, section, text, key string, who Actor, r Receipt, err error) {
	line := auditLine{Op: "remember", Scope: scope, Actor: who, ID: r.ID, Section: section, Bytes: len([]byte(text)), New: r.New, Err: errText(err), IdempotencyKey: key}
	if err == nil && key != "" {
		line.Receipt = &r
	}
	s.audit(line)
	if err == nil && r.New && s.retriever != nil {
		if ierr := s.retriever.Index(ctx, Item{ID: r.ID, Scope: scope, Section: section, Text: text}); ierr != nil {
			s.audit(auditLine{Op: "index", Scope: scope, Actor: who, ID: r.ID, Err: ierr.Error()})
		}
	}
}

// Recall asks the retriever when there is one and it answers; the
// store's keyword match otherwise.
func (s *Service) Recall(ctx context.Context, scope Scope, query string, limit int) ([]Hit, string, error) {
	if s.retriever != nil {
		if authority, ok := s.store.(interface {
			CheckRead(context.Context, Scope) error
		}); ok {
			if err := authority.CheckRead(ctx, scope); err != nil {
				return nil, "", err
			}
		}
		hits, err := s.retriever.Recall(ctx, scope, query, limit)
		if err == nil {
			return hits, s.retriever.Name(), nil
		}
		s.audit(auditLine{Op: "recall", Scope: scope, Err: err.Error()})
	}
	hits, err := s.store.Recall(ctx, scope, query, limit)
	source := "markdown"
	if named, ok := s.store.(interface{ Name() string }); ok {
		source = named.Name()
	}
	return hits, source, err
}

func (s *Service) Forget(ctx context.Context, scope Scope, id string, who Actor) (Item, error) {
	ctx = withActor(ctx, who)
	item, err := s.store.Forget(ctx, scope, id)
	s.audit(auditLine{Op: "forget", Scope: scope, Actor: who, ID: id, Err: errText(err)})
	if err == nil && s.retriever != nil {
		if derr := s.retriever.Drop(ctx, item); derr != nil {
			s.audit(auditLine{Op: "drop", Scope: scope, Actor: who, ID: id, Err: derr.Error()})
		}
	}
	return item, err
}

type auditLine struct {
	At             string   `json:"at"`
	Op             string   `json:"op"`
	Scope          Scope    `json:"scope"`
	Actor          Actor    `json:"actor"`
	ID             string   `json:"id,omitempty"`
	Section        string   `json:"section,omitempty"`
	Bytes          int      `json:"bytes,omitempty"`
	New            bool     `json:"new,omitempty"`
	Err            string   `json:"err,omitempty"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
	Receipt        *Receipt `json:"receipt,omitempty"`
}

// audit appends one line; the fact's text is not in it, only its size.
func (s *Service) audit(line auditLine) {
	if sink, ok := s.store.(interface{ recordAudit(auditLine) error }); ok {
		_ = sink.recordAudit(line)
		return
	}
	if s.auditPath == "" {
		return
	}
	line.At = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.Marshal(line)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(s.auditPath), 0o700)
	f, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------- markdown store

// Markdown keeps a scope as one file: the global one is the home's
// MEMORY.md, a project's is <dir>/projects/<id>.md. Each fact is a
// bullet; a bullet Steve wrote carries its id in a trailing comment, a
// bullet the owner typed gets a content hash until Steve rewrites it.
// Every change takes the scope's lock and recovers any pending keyed write
// before editing. Facts, pending writes and receipts use durable replacements.
type Markdown struct {
	HomePath string
	Dir      string
}

// NewMarkdown makes the store; dir is where project memories go.
func NewMarkdown(homePath, dir string) *Markdown { return &Markdown{HomePath: homePath, Dir: dir} }

// Path is where a scope's file is.
func (m *Markdown) Path(scope Scope) (string, error) {
	switch scope.Kind {
	case KindGlobal:
		if m.HomePath == "" {
			return "", errors.New("no home directory is configured (gateway.home_path); global memory has nowhere to live")
		}
		return filepath.Join(m.HomePath, home.FileMemory), nil
	case KindProject:
		id := scope.Project
		if id == "" || strings.ContainsAny(id, "/\\ \t\n") || strings.HasPrefix(id, ".") {
			return "", fmt.Errorf("bad project scope %q", scope)
		}
		return filepath.Join(m.Dir, "projects", id+".md"), nil
	}
	return "", fmt.Errorf("bad scope %q", scope)
}

func (m *Markdown) lock(scope Scope) (func(), error) {
	dir := m.Dir
	if scope.Kind == KindGlobal {
		dir = m.HomePath
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return acquire(filepath.Join(dir, ".memory.lock"))
}

func (m *Markdown) read(scope Scope) (string, error) {
	path, err := m.Path(scope)
	if err != nil {
		return "", err
	}
	if scope.Kind == KindGlobal {
		files, err := home.Files(m.HomePath)
		if err != nil {
			return "", err
		}
		for _, f := range files {
			if f.Name == home.FileMemory {
				return f.Text, nil
			}
		}
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(raw), err
}

func (m *Markdown) write(scope Scope, text string) error {
	path, err := m.writeTarget(scope)
	if err != nil {
		return err
	}
	if n := len([]byte(text)); n > Budget(scope) {
		return fmt.Errorf("memory would be %d bytes; only %d reach the agent — shorten it", n, Budget(scope))
	}
	return atomicMemoryFile(path, []byte(text))
}

// Keep global MEMORY.md inside the resolved home, including an existing
// in-home symlink, while using the same durable writer as project memory.
func (m *Markdown) writeTarget(scope Scope) (string, error) {
	path, err := m.Path(scope)
	if err != nil || scope.Kind != KindGlobal {
		return path, err
	}
	root, err := filepath.EvalSymlinks(m.HomePath)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, home.FileMemory)
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return target, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return target, nil
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", home.ErrEscape
	}
	return resolved, nil
}

// Snapshot is the file without the id comments; an untouched template
// is nothing.
func (m *Markdown) Snapshot(_ context.Context, scope Scope) (string, int, error) {
	body, err := m.read(scope)
	if err != nil {
		return "", 0, err
	}
	if scope.Kind == KindProject && !hasFacts(body) {
		return "", 0, nil
	}
	if scope.Kind == KindGlobal && home.IsTemplate(body) && !hasFacts(body) {
		return "", 0, nil
	}
	clean := stripIDs(body)
	return clean, len([]byte(clean)), nil
}

// Replace is the whole file at once, under the lock. The page edits
// the file without its id comments, so a bullet that still reads as a
// fact the file already had keeps that fact's id.
func (m *Markdown) Replace(_ context.Context, scope Scope, text string) error {
	if strings.Contains(text, "<!-- m:") {
		return errors.New("the text carries id markers; edit the plain text, ids are kept for you")
	}
	unlock, err := m.lock(scope)
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.recoverRemember(scope, time.Now()); err != nil {
		return err
	}
	old, err := m.read(scope)
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for _, it := range parse(scope, old) {
		if !strings.HasPrefix(it.ID, "h") {
			ids[hashOf(it.Text)] = it.ID
		}
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		it, ok := parseBullet(scope, "", line)
		if !ok {
			continue
		}
		if id, found := ids[hashOf(it.Text)]; found {
			lines[i] = strings.TrimRight(line, " \t") + " " + idComment(id)
		}
	}
	return m.write(scope, strings.Join(lines, "\n"))
}

// Text is the file as the page edits it: no id comments, the template
// when there is nothing yet.
func (m *Markdown) Text(_ context.Context, scope Scope) (string, error) {
	body, err := m.read(scope)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == "" {
		return Template(scope), nil
	}
	return stripIDs(body), nil
}

func (m *Markdown) List(_ context.Context, scope Scope) ([]Item, error) {
	body, err := m.read(scope)
	if err != nil {
		return nil, err
	}
	return parse(scope, body), nil
}

// Remember adds one bullet under a section, creating the file when there
// is none. The same fact twice is one fact.
func (m *Markdown) Remember(_ context.Context, scope Scope, section, text string) (Receipt, error) {
	unlock, err := m.lock(scope)
	if err != nil {
		return Receipt{}, err
	}
	defer unlock()
	if err := m.recoverRemember(scope, time.Now()); err != nil {
		return Receipt{}, err
	}
	body, err := m.read(scope)
	if err != nil {
		return Receipt{}, err
	}
	after, r, err := prepareRemember(scope, section, text, body)
	if err != nil {
		return Receipt{}, err
	}
	if r.New {
		if err := m.write(scope, after); err != nil {
			return Receipt{}, err
		}
	}
	return r, nil
}

func prepareRemember(scope Scope, section, text, body string) (string, Receipt, error) {
	text = strings.TrimSpace(spaces.ReplaceAllString(text, " "))
	if text == "" {
		return "", Receipt{}, errors.New("nothing to remember")
	}
	if len([]rune(text)) > MaxFact {
		return "", Receipt{}, fmt.Errorf("a memory is a short fact; keep it under %d characters", MaxFact)
	}
	if strings.Contains(text, "<!--") {
		return "", Receipt{}, errors.New("a memory cannot contain a comment marker")
	}
	section = normalizeSection(scope, section)
	if strings.TrimSpace(body) == "" || (scope.Kind == KindGlobal && home.IsTemplate(body) && !hasFacts(body)) {
		body = Template(scope)
	}
	key := hashOf(text)
	for _, it := range parse(scope, body) {
		if hashOf(it.Text) == key {
			return body, Receipt{ID: it.ID, New: false, Bytes: len([]byte(stripIDs(body))), Budget: Budget(scope)}, nil
		}
	}
	id := newID()
	body = insertBullet(body, section, "- "+text+" "+idComment(id))
	if len([]byte(body)) > Budget(scope) {
		return "", Receipt{}, fmt.Errorf("memory would be %d bytes; only %d reach the agent — shorten it", len([]byte(body)), Budget(scope))
	}
	return body, Receipt{ID: id, New: true, Bytes: len([]byte(stripIDs(body))), Budget: Budget(scope)}, nil
}

// Recall scores every fact by how many of the query's words it holds.
func (m *Markdown) Recall(ctx context.Context, scope Scope, query string, limit int) ([]Hit, error) {
	items, err := m.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	return rankMemory(items, query, limit), nil
}

func rankMemory(items []Item, query string, limit int) []Hit {
	words := tokens(query)
	var hits []Hit
	for _, it := range items {
		score := 0.0
		if len(words) == 0 {
			score = 1
		} else {
			lower := strings.ToLower(it.Text)
			for _, w := range words {
				if strings.Contains(lower, w) {
					score++
				}
			}
			score /= float64(len(words))
		}
		if score > 0 {
			hits = append(hits, Hit{Item: it, Score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// Forget drops the bullet with the id.
func (m *Markdown) Forget(_ context.Context, scope Scope, id string) (Item, error) {
	unlock, err := m.lock(scope)
	if err != nil {
		return Item{}, err
	}
	defer unlock()
	if err := m.recoverRemember(scope, time.Now()); err != nil {
		return Item{}, err
	}
	body, err := m.read(scope)
	if err != nil {
		return Item{}, err
	}
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	var found *Item
	for _, line := range lines {
		if it, ok := parseBullet(scope, "", line); ok && it.ID == id {
			found = &it
			continue
		}
		kept = append(kept, line)
	}
	if found == nil {
		return Item{}, fmt.Errorf("no memory %s in %s", id, scope)
	}
	return *found, m.write(scope, strings.Join(kept, "\n"))
}

// ---------------------------------------------------------------- markdown shape

// Template is the file a scope starts from.
func Template(scope Scope) string {
	var b strings.Builder
	if scope.Kind == KindProject {
		fmt.Fprintf(&b, "# Memory · %s\n\n这个项目长期仍然为真的事：约定、决策、踩过的坑。短。重要的放上面。\n", scope.Project)
	} else {
		b.WriteString("# Memory\n\n只写仍然为真的长期事实。短。重要的放上面。\n")
	}
	for _, s := range Sections(scope) {
		b.WriteString("\n## " + s + "\n")
	}
	return b.String()
}

var sectionAliases = map[string]string{
	"preference": "偏好", "preferences": "偏好", "pref": "偏好", "habit": "偏好",
	"project": "项目", "projects": "项目", "work": "项目",
	"people": "人", "person": "人", "contacts": "人", "who": "人",
	"convention": "约定", "conventions": "约定", "rule": "约定", "rules": "约定", "agreement": "约定",
	"decision": "决策", "decisions": "决策", "why": "决策",
	"pitfall": "坑", "pitfalls": "坑", "gotcha": "坑", "gotchas": "坑", "trap": "坑", "踩坑": "坑",
}

// normalizeSection maps what the agent said to one of the scope's
// headings; anything else goes under the first.
func normalizeSection(scope Scope, s string) string {
	s = strings.TrimSpace(s)
	sections := Sections(scope)
	for _, want := range sections {
		if s == want {
			return want
		}
	}
	if v, ok := sectionAliases[strings.ToLower(s)]; ok {
		for _, want := range sections {
			if v == want {
				return v
			}
		}
	}
	return sections[0]
}

var (
	spaces    = regexp.MustCompile(`\s+`)
	idPattern = regexp.MustCompile(`\s*<!--\s*m:([0-9a-f]{12})\s*-->\s*$`)
)

func idComment(id string) string { return "<!-- m:" + id + " -->" }

func newID() string {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// hashOf is the fact's normalised text hashed: case folded, whitespace
// collapsed, trailing punctuation dropped. Owner-typed bullets use it as
// their id, so the same line keeps the same id until it is edited.
func hashOf(text string) string {
	n := strings.ToLower(spaces.ReplaceAllString(strings.TrimSpace(text), " "))
	n = strings.TrimRight(n, "。．.!！?？;；,，")
	sum := sha256.Sum256([]byte(n))
	return "h" + hex.EncodeToString(sum[:])[:11]
}

func isBullet(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")
}

// parseBullet reads one bullet line into an item.
func parseBullet(scope Scope, section, line string) (Item, bool) {
	if !isBullet(line) {
		return Item{}, false
	}
	t := strings.TrimSpace(line)[2:]
	id := ""
	if m := idPattern.FindStringSubmatch(t); m != nil {
		id = m[1]
		t = strings.TrimSpace(t[:len(t)-len(m[0])])
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return Item{}, false
	}
	if id == "" {
		id = hashOf(t)
	}
	return Item{ID: id, Scope: scope, Section: section, Text: t}, true
}

func parse(scope Scope, body string) []Item {
	var out []Item
	section := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "## ") {
			section = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "## "))
			continue
		}
		if it, ok := parseBullet(scope, section, line); ok {
			out = append(out, it)
		}
	}
	return out
}

func hasFacts(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if _, ok := parseBullet(Scope{}, "", line); ok {
			return true
		}
	}
	return false
}

// stripIDs is the file as a reader wants it: no id comments.
func stripIDs(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if isBullet(line) {
			lines[i] = idPattern.ReplaceAllString(line, "")
		}
	}
	return strings.Join(lines, "\n")
}

// insertBullet appends a bullet at the end of a section's bullets; a
// section missing from the file is added at the end.
func insertBullet(body, section, bullet string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	header := "## " + section
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			start = i
			break
		}
	}
	if start < 0 {
		return strings.Join(lines, "\n") + "\n\n" + header + "\n\n" + bullet + "\n"
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	last := -1
	for i := start + 1; i < end; i++ {
		if isBullet(lines[i]) {
			last = i
		}
	}
	var out []string
	if last >= 0 {
		out = append(out, lines[:last+1]...)
		out = append(out, bullet)
		out = append(out, lines[last+1:]...)
		return strings.Join(out, "\n") + "\n"
	}
	// No bullets yet: header, blank, bullet, then the rest with one
	// blank before whatever follows.
	rest := lines[start+1:]
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	out = append(out, lines[:start+1]...)
	out = append(out, "", bullet)
	if len(rest) > 0 {
		out = append(out, "")
		out = append(out, rest...)
	}
	return strings.Join(out, "\n") + "\n"
}

// truncate cuts to a byte budget on a line boundary.
func truncate(s string, budget int) string {
	if len([]byte(s)) <= budget {
		return s
	}
	b := []byte(s)[:budget]
	if i := strings.LastIndexByte(string(b), '\n'); i > 0 {
		b = b[:i]
	}
	return strings.TrimRight(strings.ToValidUTF8(string(b), ""), "\n") + "\n"
}

// tokens splits a query into lowercase words; a run of CJK is split
// into pairs so a substring still matches.
func tokens(q string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		if isCJK(f) && len([]rune(f)) > 2 {
			r := []rune(f)
			for i := 0; i+1 < len(r); i++ {
				out = append(out, string(r[i:i+2]))
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

func isCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
