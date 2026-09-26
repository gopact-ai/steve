package sshconnect

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/i18n"
)

// BrowseRequest names a directory on a machine: one behind an SSH alias
// while it is being enrolled, or one already in the cluster, named by node
// ID, whose alias the backend knows. An empty path means the account's
// home.
type BrowseRequest struct {
	Alias string `json:"alias,omitempty"`
	Node  string `json:"node,omitempty"`
	Path  string `json:"path"`
}

// AliasBackend is offered by a backend that knows which SSH alias an
// enrolled machine is reached through.
type AliasBackend interface {
	MachineAlias(ctx context.Context, nodeID string) (string, error)
}

// Listing is what the machine reported about one directory: where it is,
// the visible directories beneath it, and whether this account could create
// a folder in it. When the requested directory does not exist yet, the
// listing is of its nearest existing ancestor and Requested names what was
// asked for, shown the way Display is.
type Listing struct {
	Path      string  `json:"path"`
	Display   string  `json:"display"`
	Home      string  `json:"home"`
	Parent    string  `json:"parent,omitempty"`
	Requested string  `json:"requested,omitempty"`
	Writable  bool    `json:"writable"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated,omitempty"`
}

// Entry is one directory inside a listing.
type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

const (
	maxBrowseEntries = 400
	browseTimeout    = 20 * time.Second
)

// Browse lists the directories under one path on the remote machine so a
// person can pick a workspace instead of typing it blind. It reads only.
func (s *Service) Browse(ctx context.Context, req BrowseRequest) (Listing, error) {
	ctx, text := s.speak(ctx)
	alias, err := s.browseAlias(ctx, text, req)
	if err != nil {
		return Listing{}, err
	}
	c, _, err := s.selected(ctx, alias)
	if err != nil {
		return Listing{}, err
	}
	target := strings.TrimSpace(req.Path)
	if target == "" {
		target = "~"
	}
	if problem := validBrowsePath(text, target); problem != "" {
		return Listing{}, Fail(text, "configuration", "invalid_path", problem, text.T(i18n.SSHBrowsePathFix))
	}
	connection, err := s.bind(ctx, c)
	if err != nil {
		return Listing{}, err
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(ctx, browseTimeout)
	defer cancel()
	output, err := connection.Run(ctx, "sh -s", browseScript(target))
	if err != nil {
		return Listing{}, connectionError(ctx, output.Stderr)
	}
	return parseListing(text, output.Stdout, target)
}

// browseAlias is the alias to reach the machine the request names: its own
// when the machine is being enrolled, or the one the backend keeps for a
// machine already in the cluster.
func (s *Service) browseAlias(ctx context.Context, text i18n.Catalog, req BrowseRequest) (string, error) {
	node := strings.TrimSpace(req.Node)
	if node == "" {
		return req.Alias, nil
	}
	if strings.TrimSpace(req.Alias) != "" {
		return "", Fail(text, "configuration", "browse_ambiguous", text.T(i18n.SSHBrowseAmbiguous), text.T(i18n.SSHBrowseAmbiguousFix))
	}
	backend, ok := s.backend.(AliasBackend)
	if !ok {
		return "", Fail(text, "configuration", "browse_unsupported", text.T(i18n.SSHBrowseUnsupported), text.T(i18n.SSHBrowseTypePathFix))
	}
	alias, err := backend.MachineAlias(ctx, node)
	if err != nil {
		return "", Fail(text, "configuration", "browse_target", err.Error(), text.T(i18n.SSHBrowseTypePathFix))
	}
	return alias, nil
}

// validBrowsePath says what is wrong with dir, in text's language, or
// nothing when it may be listed.
func validBrowsePath(text i18n.Catalog, dir string) string {
	if len(dir) > 512 || strings.ContainsAny(dir, "\r\n\x00\t") || !utf8.ValidString(dir) {
		return text.T(i18n.SSHBrowsePathChars)
	}
	if !strings.HasPrefix(dir, "/") && dir != "~" && !strings.HasPrefix(dir, "~/") {
		return text.T(i18n.SSHBrowsePathAbsolute)
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." {
			return text.T(i18n.SSHBrowsePathParent)
		}
	}
	return ""
}

// browseScript lists one directory in a single round trip. The path travels
// as data: printf rebuilds it from octal escapes, so a name that looks like
// shell syntax is still just a name. A directory that does not exist yet
// opens at its nearest existing ancestor, and the listing stops after
// maxBrowseEntries so the end marker always fits in the bounded output.
func browseScript(target string) string {
	var escaped strings.Builder
	for _, b := range []byte(target) {
		fmt.Fprintf(&escaped, "\\%03o", b)
	}
	return `target=$(printf '` + escaped.String() + `')
case "$target" in
  '~') target=$HOME ;;
  '~/'*) target=$HOME/${target#'~/'} ;;
esac
printf 'STEVE_BROWSE\thome\t%s\n' "$HOME"
requested=$target
while ! cd -- "$target" 2>/dev/null; do
  if [ "$target" = / ]; then
    printf 'STEVE_BROWSE\tmissing\t1\n'
    printf 'STEVE_BROWSE\tend\t1\n'
    exit 0
  fi
  target=${target%/*}
  [ -n "$target" ] || target=/
done
if [ "$target" != "$requested" ]; then printf 'STEVE_BROWSE\trequested\t%s\n' "$requested"; fi
printf 'STEVE_BROWSE\tpath\t%s\n' "$PWD"
if [ -w . ]; then printf 'STEVE_BROWSE\twritable\t1\n'; fi
count=0
for entry in *; do
  [ -d "$entry" ] || continue
  count=$((count + 1))
  if [ "$count" -gt ` + fmt.Sprint(maxBrowseEntries) + ` ]; then
    printf 'STEVE_BROWSE\ttruncated\t1\n'
    break
  fi
  printf 'STEVE_BROWSE\tdir\t%s\n' "$entry"
done
printf 'STEVE_BROWSE\tend\t1\n'
`
}

func parseListing(text i18n.Catalog, output, requested string) (Listing, error) {
	listing := Listing{Entries: []Entry{}}
	var missing, ended bool
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(fields) != 3 || fields[0] != "STEVE_BROWSE" {
			continue
		}
		switch key, value := fields[1], fields[2]; key {
		case "home":
			listing.Home = value
		case "path":
			listing.Path = value
		case "writable":
			listing.Writable = value == "1"
		case "missing":
			missing = value == "1"
		case "requested":
			listing.Requested = value
		case "truncated":
			listing.Truncated = value == "1"
		case "end":
			ended = value == "1"
		case "dir":
			if value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\r\n\x00") {
				listing.Entries = append(listing.Entries, Entry{Name: value})
			}
		}
	}
	if !ended {
		return Listing{}, Fail(text, "environment", "invalid_listing", text.T(i18n.SSHBrowseIncomplete), text.T(i18n.SSHBrowseIncompleteFix))
	}
	if missing || listing.Path == "" {
		return Listing{}, Fail(text, "environment", "directory_unavailable", text.T(i18n.SSHBrowseUnreachable, requested), text.T(i18n.SSHBrowseUnreachableFix))
	}
	sort.Slice(listing.Entries, func(i, j int) bool {
		return strings.ToLower(listing.Entries[i].Name) < strings.ToLower(listing.Entries[j].Name)
	})
	for i := range listing.Entries {
		listing.Entries[i].Path = path.Join(listing.Path, listing.Entries[i].Name)
	}
	if listing.Path != "/" {
		listing.Parent = path.Dir(listing.Path)
	}
	if listing.Home != "" {
		listing.Home = path.Clean(listing.Home)
	}
	listing.Display = displayPath(listing.Path, listing.Home)
	if listing.Requested != "" {
		listing.Requested = displayPath(listing.Requested, listing.Home)
	}
	return listing, nil
}

// displayPath shows the account home as ~, the way the workspace field
// expects it to be written.
func displayPath(dir, home string) string {
	dir = path.Clean(dir)
	if home != "" {
		home = path.Clean(home)
	}
	switch {
	case home == "" || home == "/":
		return dir
	case dir == home:
		return "~"
	case strings.HasPrefix(dir, home+"/"):
		return "~" + strings.TrimPrefix(dir, home)
	}
	return dir
}
