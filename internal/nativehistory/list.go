package nativehistory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// List reads bounded metadata from native histories without loading a session.
// A missing history tree is an empty list; unreadable or malformed source
// records return an error instead of advertising them as importable.
func List(ctx context.Context, source Source) ([]Entry, error) {
	root, err := sourceRoot(source)
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	base := "sessions"
	if source.Harness == "claude-code" {
		base = "projects"
	}
	var out []Entry
	err = fs.WalkDir(root.FS(), base, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(walkErr, os.ErrNotExist) && path == base {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		switch source.Harness {
		case "codex":
			if !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
		case "claude-code":
			if len(strings.Split(path, "/")) != 3 || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		entry, err := readEntry(root, source, path)
		if err != nil {
			return err
		}
		if !validSourceNativeID(source.Harness, entry.NativeID) || !filepath.IsAbs(entry.Workdir) {
			return nil
		}
		if len(out) >= maxEntries {
			return errors.New("native session list exceeds 2000 records; select a narrower history home")
		}
		entry.UpdatedAt = info.ModTime().UTC()
		files, err := inventory(ctx, root, entry)
		if err != nil {
			return err
		}
		entry.Revision = inventoryRevision(entry, files)
		out = append(out, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Entry) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.NativeID, b.NativeID)
	})
	if out == nil {
		out = []Entry{}
	}
	return out, nil
}

func readEntry(root *os.Root, source Source, path string) (Entry, error) {
	entry := Entry{Harness: source.Harness, SourceHome: source.Home, path: path}
	file, err := root.Open(path)
	if err != nil {
		return entry, err
	}
	defer file.Close()
	scan := bufio.NewScanner(io.LimitReader(file, 8<<20))
	scan.Buffer(make([]byte, 64<<10), 8<<20)
	for scan.Scan() {
		var row struct {
			Type        string `json:"type"`
			SessionID   string `json:"sessionId"`
			Cwd         string `json:"cwd"`
			CustomTitle string `json:"customTitle"`
			Payload     struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			} `json:"payload"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			return entry, err
		}
		if source.Harness == "codex" {
			if row.Type == "session_meta" {
				entry.NativeID, entry.Workdir = row.Payload.ID, row.Payload.Cwd
				return entry, nil
			}
			continue
		}
		if row.SessionID != "" {
			entry.NativeID = row.SessionID
		}
		if row.Cwd != "" {
			entry.Workdir = row.Cwd
		}
		if row.CustomTitle != "" {
			entry.Title = clipped(row.CustomTitle)
		}
		if row.Type == "user" && entry.Title == "" {
			entry.Title = messageTitle(row.Message.Content)
		}
		if entry.NativeID != "" && entry.Workdir != "" && entry.Title != "" {
			return entry, nil
		}
	}
	return entry, scan.Err()
}

func messageTitle(raw json.RawMessage) string {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return clipped(plain)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				return clipped(b.Text)
			}
		}
	}
	return ""
}
