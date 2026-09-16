package sshconnect

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// BrowseRequest names a directory on the machine behind an SSH alias. An
// empty path means the account's home.
type BrowseRequest struct {
	Alias string `json:"alias"`
	Path  string `json:"path"`
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
	c, _, err := s.selected(ctx, req.Alias)
	if err != nil {
		return Listing{}, err
	}
	target := strings.TrimSpace(req.Path)
	if target == "" {
		target = "~"
	}
	if err := validBrowsePath(target); err != nil {
		return Listing{}, fail("configuration", "invalid_path", err.Error(), "填写目标机上的绝对路径，或以 ~/ 开头的路径")
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
	return parseListing(output.Stdout, target)
}

func validBrowsePath(dir string) error {
	if len(dir) > 512 || strings.ContainsAny(dir, "\r\n\x00\t") || !utf8.ValidString(dir) {
		return fmt.Errorf("目录路径包含无效字符")
	}
	if !strings.HasPrefix(dir, "/") && dir != "~" && !strings.HasPrefix(dir, "~/") {
		return fmt.Errorf("目录要写目标机上的绝对路径，或以 ~/ 开头")
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." {
			return fmt.Errorf("目录路径不能包含 ..")
		}
	}
	return nil
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

func parseListing(output, requested string) (Listing, error) {
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
		return Listing{}, fail("environment", "invalid_listing", "SSH 已连接，但未收到完整的目录列表", "确认该账号允许运行标准 POSIX shell 后重试")
	}
	if missing || listing.Path == "" {
		return Listing{}, fail("environment", "directory_unavailable", "当前账号进不了目标机上 "+requested+" 及其任何上级目录", "换一个这个账号能进入的目录")
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
