package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Entry is one name in a snapshot's directory.
type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"` // file | dir | link | repo
	Size int64  `json:"size,omitempty"`
	Mode string `json:"mode,omitempty"`
}

// Limits on browsing: a directory lists at most this many names, a
// file is read up to this many bytes.
const (
	MaxEntries   = 2000
	MaxFileBytes = 200 * 1024
)

// Tree lists one directory of a snapshot, no deeper: files with their
// sizes, directories, links and nested repositories as what they are.
func (r *Repo) Tree(ctx context.Context, commit, dir string) ([]Entry, bool, error) {
	if !shaPattern.MatchString(commit) {
		return nil, false, fmt.Errorf("bad snapshot id %q", commit)
	}
	dir = cleanPath(dir)
	spec := commit
	if dir != "" {
		spec = commit + ":" + dir
	}
	ctx, cancel := context.WithTimeout(ctx, r.Review.WithDefaults().Timeout)
	defer cancel()
	out, err := r.Git(ctx, nil, "ls-tree", "-z", "-l", spec)
	if err != nil {
		return nil, false, err
	}
	var entries []Entry
	truncated := false
	for _, rec := range strings.Split(out, "\x00") {
		// "<mode> <type> <object> <size>\t<name>"
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 4 {
			continue
		}
		if len(entries) >= r.Review.WithDefaults().MaxEntries {
			truncated = true
			break
		}
		e := Entry{Name: name, Mode: fields[0]}
		if dir != "" {
			e.Path = dir + "/" + name
		} else {
			e.Path = name
		}
		switch {
		case fields[1] == "tree":
			e.Kind = "dir"
		case fields[1] == "commit":
			e.Kind = "repo"
		case fields[0] == "120000":
			e.Kind = "link"
		default:
			e.Kind = "file"
			size, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return nil, false, fmt.Errorf("ls-tree size of %s: %w", e.Path, err)
			}
			e.Size = size
		}
		entries = append(entries, e)
	}
	return entries, truncated, nil
}

// File is one file of a snapshot: its text up to MaxFileBytes, or only
// its size when it is binary. A link shows its target.
func (r *Repo) File(ctx context.Context, commit, path string) (text string, size int64, binary, truncated bool, err error) {
	if !shaPattern.MatchString(commit) {
		return "", 0, false, false, fmt.Errorf("bad snapshot id %q", commit)
	}
	path = cleanPath(path)
	if path == "" {
		return "", 0, false, false, errors.New("a path is required")
	}
	ctx, cancel := context.WithTimeout(ctx, r.Review.WithDefaults().Timeout)
	defer cancel()
	spec := commit + ":" + path
	kind, err := r.Git(ctx, nil, "cat-file", "-t", spec)
	if err != nil {
		return "", 0, false, false, err
	}
	if strings.TrimSpace(kind) != "blob" {
		return "", 0, false, false, fmt.Errorf("%s is a %s, not a file", path, strings.TrimSpace(kind))
	}
	sizeText, err := r.Git(ctx, nil, "cat-file", "-s", spec)
	if err != nil {
		return "", 0, false, false, err
	}
	if size, err = strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64); err != nil {
		return "", 0, false, false, fmt.Errorf("size of %s: %w", path, err)
	}
	raw, _, err := r.GitBounded(ctx, r.Review.WithDefaults().MaxFileBytes+1, "cat-file", "-p", spec)
	if err != nil {
		return "", size, false, false, err
	}
	truncated = int64(len(raw)) > int64(r.Review.WithDefaults().MaxFileBytes) || size > int64(r.Review.WithDefaults().MaxFileBytes)
	if len(raw) > r.Review.WithDefaults().MaxFileBytes {
		raw = raw[:r.Review.WithDefaults().MaxFileBytes]
	}
	if truncated && len(raw) > 0 {
		start := len(raw) - 1
		for start > 0 && !utf8.RuneStart(raw[start]) {
			start--
		}
		if !utf8.FullRune(raw[start:]) {
			raw = raw[:start]
		}
	}
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return "", size, true, false, nil
	}
	return string(raw), size, false, truncated, nil
}

// GitBounded runs git and returns at most max bytes of its stdout,
// stopping the process once the limit is read; truncated says it did.
func (r *Repo) GitBounded(ctx context.Context, max int, args ...string) (out []byte, truncated bool, err error) {
	return runBounded(ctx, r.Dir, max, args...)
}

// cleanPath keeps a path inside the tree: no leading slash, no "..",
// no empty segments.
func cleanPath(p string) string {
	var parts []string
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, "/")
}
