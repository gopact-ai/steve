package skills

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// A Source is a git repository skills are installed from: a GitHub
// address, an owner/repo shorthand, or a tree link into a subdirectory.
// The hub clones it shallowly under the state directory, finds the
// skills in it, and lists them through a root of links that is one of
// the map's search paths — so an installed skill is enabled, shipped
// and shown like any other. Update fetches again; remove forgets the
// clone.
type Source struct {
	Slug      string    `json:"slug"`
	URL       string    `json:"url"`
	Ref       string    `json:"ref,omitempty"`
	Subdir    string    `json:"subdir,omitempty"`
	Dir       string    `json:"dir"`
	Root      string    `json:"root"`
	Head      string    `json:"head,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	Skills    []string  `json:"skills"`
	Error     string    `json:"error,omitempty"`
}

var (
	githubTree  = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/(?:tree|blob)/([^/]+)/(.+?)/?$`)
	githubRepo  = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`)
	shorthand   = regexp.MustCompile(`^([\w.-]+)/([\w.-]+?)(?:\.git)?$`)
	shorthandIn = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)/(.+?)/?$`)
	slugBad     = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// ParseSourceSpec reads what the owner typed. Accepted: a GitHub tree
// link (repository, ref and subdirectory), a GitHub repository URL,
// owner/repo, owner/repo/path, a git URL (https, ssh, git@, file).
func ParseSourceSpec(spec string) (Source, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Source{}, fmt.Errorf("a source is a git repository: a GitHub link, owner/repo, or a tree link into a subdirectory")
	}
	var s Source
	switch {
	case githubTree.MatchString(spec):
		m := githubTree.FindStringSubmatch(spec)
		s = Source{URL: "https://github.com/" + m[1] + "/" + m[2] + ".git", Ref: m[3], Subdir: strings.Trim(m[4], "/")}
	case githubRepo.MatchString(spec):
		m := githubRepo.FindStringSubmatch(spec)
		s = Source{URL: "https://github.com/" + m[1] + "/" + m[2] + ".git"}
	case strings.Contains(spec, "://") || strings.HasPrefix(spec, "git@"):
		s = Source{URL: spec}
	case shorthand.MatchString(spec):
		m := shorthand.FindStringSubmatch(spec)
		s = Source{URL: "https://github.com/" + m[1] + "/" + m[2] + ".git"}
	case shorthandIn.MatchString(spec):
		m := shorthandIn.FindStringSubmatch(spec)
		s = Source{URL: "https://github.com/" + m[1] + "/" + m[2] + ".git", Subdir: strings.Trim(m[3], "/")}
	default:
		return Source{}, fmt.Errorf("%q is not a git source: use a GitHub link, owner/repo, or a tree link into a subdirectory", spec)
	}
	if strings.Contains(s.Subdir, "..") {
		return Source{}, fmt.Errorf("subdirectory %q may not climb out of the repository", s.Subdir)
	}
	s.Slug = slugOf(s)
	return s, nil
}

// slugOf names the clone: host, owner, repository, subdirectory.
func slugOf(s Source) string {
	name := s.URL
	if u, err := url.Parse(s.URL); err == nil && u.Host != "" {
		name = u.Host + "/" + strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	} else if i := strings.Index(name, ":"); strings.HasPrefix(name, "git@") && i > 0 {
		name = name[4:i] + "/" + strings.TrimSuffix(name[i+1:], ".git")
	}
	if s.Subdir != "" {
		name += "/" + s.Subdir
	}
	return strings.Trim(slugBad.ReplaceAllString(name, "-"), "-")
}

// sourcesDir is where clones live: beside the map file.
func (m *Map) sourcesDir() string { return filepath.Join(filepath.Dir(m.path), "skills-sources") }

// Sources lists the installed sources.
func (m *Map) Sources() []Source {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return nil
	}
	out := append([]Source{}, data.Sources...)
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// AddSource clones a source, finds its skills and makes them available.
// The skills are not enabled: that is a choice made per skill.
func (m *Map) AddSource(ctx context.Context, spec string) (Source, error) {
	s, err := ParseSourceSpec(spec)
	if err != nil {
		return Source{}, err
	}
	m.mu.Lock()
	data, err := m.readLocked()
	m.mu.Unlock()
	if err != nil {
		return Source{}, err
	}
	for _, have := range data.Sources {
		if have.Slug == s.Slug {
			return have, fmt.Errorf("%s is already installed; update it instead", s.Slug)
		}
	}
	s.Dir = filepath.Join(m.sourcesDir(), s.Slug)
	s.Root = s.Dir + ".skills"
	if err := os.MkdirAll(m.sourcesDir(), 0o700); err != nil {
		return Source{}, err
	}
	if err := gitClone(ctx, s); err != nil {
		return Source{}, err
	}
	if err := m.refreshSource(&s); err != nil {
		// The source is not recorded, so the clone and its root are only
		// disk: the refresh error is the answer, and the next install of
		// the same source clears whatever a failed remove leaves.
		_ = os.RemoveAll(s.Dir)
		_ = os.RemoveAll(s.Root)
		return Source{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err = m.readLocked()
	if err != nil {
		return Source{}, err
	}
	data.Sources = append(data.Sources, s)
	if !slices.Contains(data.SearchPaths, s.Root) {
		data.SearchPaths = append(data.SearchPaths, s.Root)
	}
	return s, m.writeLocked(data)
}

// UpdateSources fetches every source again and relinks its skills. A
// source that fails to fetch keeps what it had and says why.
func (m *Map) UpdateSources(ctx context.Context) ([]Source, error) {
	m.mu.Lock()
	data, err := m.readLocked()
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]Source, 0, len(data.Sources))
	for _, s := range data.Sources {
		s.Error = ""
		if err := gitUpdate(ctx, s); err != nil {
			s.Error = err.Error()
		} else if err := m.refreshSource(&s); err != nil {
			s.Error = err.Error()
		}
		out = append(out, s)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err = m.readLocked()
	if err != nil {
		return nil, err
	}
	data.Sources = out
	return out, m.writeLocked(data)
}

// RemoveSource forgets a source: its root leaves the search paths, its
// clone is deleted, and skills that were enabled from it are turned
// off, since there is nothing left to enable.
func (m *Map) RemoveSource(slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.readLocked()
	if err != nil {
		return err
	}
	var gone *Source
	kept := data.Sources[:0]
	for _, s := range data.Sources {
		if s.Slug == slug {
			g := s
			gone = &g
			continue
		}
		kept = append(kept, s)
	}
	if gone == nil {
		return fmt.Errorf("no source %q", slug)
	}
	data.Sources = kept
	paths := data.SearchPaths[:0]
	for _, p := range data.SearchPaths {
		if p != gone.Root {
			paths = append(paths, p)
		}
	}
	data.SearchPaths = paths
	// The source is forgotten either way; a clone that will not go is only
	// disk, but the owner asked for it to be gone, so say so. Both removes
	// precede the enabled filter below because a skill enabled by absolute
	// path into the clone is resolved on the filesystem.
	for _, dir := range []string{gone.Root, gone.Dir} {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn(fmt.Sprintf("skills: remove source %s: %v", slug, err), "source", slug, "dir", dir)
		}
	}
	enabled := data.Enabled[:0]
	for _, name := range data.Enabled {
		if _, err := m.resolveLocked(data, name); err == nil {
			enabled = append(enabled, name)
		}
	}
	data.Enabled = enabled
	return m.writeLocked(data)
}

// refreshSource reads the clone: its head, the skills in it, and the
// root of links that lists them.
func (m *Map) refreshSource(s *Source) error {
	head, err := gitOutput(context.Background(), s.Dir, "rev-parse", "--short=12", "HEAD")
	if err == nil {
		s.Head = strings.TrimSpace(head)
	}
	s.FetchedAt = time.Now().UTC()
	base := s.Dir
	if s.Subdir != "" {
		base = filepath.Join(s.Dir, filepath.FromSlash(s.Subdir))
	}
	found := map[string]string{}
	if hasSkill(base) {
		name := filepath.Base(base)
		if s.Subdir == "" {
			name = strings.TrimSuffix(filepath.Base(s.Slug), ".git")
			if i := strings.LastIndex(s.Slug, "-"); i > 0 {
				name = s.Slug[i+1:]
			}
		}
		found[name] = base
	} else {
		// A repository's skills sit in its directories or under skills/;
		// a collection like anthropics/skills has both (a template at the
		// top, the skills below), so both are read.
		for _, dir := range []string{base, filepath.Join(base, "skills")} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if hasSkill(filepath.Join(dir, e.Name())) {
					if _, dup := found[e.Name()]; !dup {
						found[e.Name()] = filepath.Join(dir, e.Name())
					}
				}
			}
		}
	}
	if len(found) == 0 {
		return fmt.Errorf("no skill in %s: no SKILL.md at the top, in its directories, or under skills/", s.Slug)
	}
	if err := os.RemoveAll(s.Root); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return err
	}
	s.Skills = s.Skills[:0]
	for name, dir := range found {
		if err := os.Symlink(dir, filepath.Join(s.Root, name)); err != nil {
			return err
		}
		s.Skills = append(s.Skills, name)
	}
	sort.Strings(s.Skills)
	return nil
}

func gitClone(ctx context.Context, s Source) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	args := []string{"clone", "--quiet", "--depth", "1", "--no-tags"}
	if s.Ref != "" {
		args = append(args, "--branch", s.Ref)
	}
	args = append(args, "--", s.URL, s.Dir)
	// Leftovers of an earlier clone: if they will not go, git refuses to
	// clone into the non-empty directory and reports it.
	_ = os.RemoveAll(s.Dir)
	if out, err := gitRun(ctx, "", args...); err != nil {
		// The clone error is the answer; the next clone clears what a
		// failed remove leaves.
		_ = os.RemoveAll(s.Dir)
		return fmt.Errorf("clone %s: %s", s.URL, strings.TrimSpace(out))
	}
	return nil
}

func gitUpdate(ctx context.Context, s Source) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	fetch := []string{"fetch", "--quiet", "--depth", "1", "--no-tags", "origin"}
	target := "@{u}"
	if s.Ref != "" {
		fetch = append(fetch, s.Ref)
		target = "FETCH_HEAD"
	}
	if out, err := gitRun(ctx, s.Dir, fetch...); err != nil {
		return fmt.Errorf("fetch: %s", strings.TrimSpace(out))
	}
	if out, err := gitRun(ctx, s.Dir, "reset", "--quiet", "--hard", target); err != nil {
		return fmt.Errorf("reset: %s", strings.TrimSpace(out))
	}
	return nil
}

func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	out, err := cmd.CombinedOutput()
	if err != nil && strings.TrimSpace(string(out)) == "" {
		return err.Error(), err
	}
	return string(out), err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
