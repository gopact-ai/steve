// Package sessions reads local coding-agent transcripts the owner opted in to share.
package sessions

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxFiles       = 24
	maxPerFile     = 8
	maxTextRunes   = 400
	maxTotalBytes  = 24 * 1024
	maxFileBytes   = 8 << 20
	skipDirName    = "subagents"
	envContextMark = "<environment_context>"
)

type Source string

const (
	SourceCodex  Source = "codex"
	SourceClaude Source = "claude"
	SourceCursor Source = "cursor"
	SourceGrok   Source = "grok"
	SourceKimi   Source = "kimi"
)

type Excerpt struct {
	Source Source
	File   string
	Texts  []string
}

func Collect(userHome string) []Excerpt {
	if userHome == "" {
		return nil
	}
	roots := []struct {
		source Source
		dir    string
	}{
		{SourceCodex, filepath.Join(userHome, ".codex", "sessions")},
		{SourceClaude, filepath.Join(userHome, ".claude", "projects")},
		{SourceCursor, filepath.Join(userHome, ".cursor", "projects")},
		{SourceGrok, filepath.Join(userHome, ".grok", "sessions")},
		{SourceKimi, filepath.Join(userHome, ".kimi-code", "sessions")},
		{SourceKimi, filepath.Join(userHome, ".kimi", "sessions")},
	}
	type dated struct {
		source Source
		path   string
		mod    time.Time
	}
	var files []dated
	for _, root := range roots {
		_ = filepath.WalkDir(root.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return fs.SkipDir
			}
			if d.IsDir() {
				if d.Name() == skipDirName {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() == 0 || info.Size() > maxFileBytes {
				return nil
			}
			files = append(files, dated{source: root.source, path: path, mod: info.ModTime()})
			return nil
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}
	var out []Excerpt
	total := 0
	for _, file := range files {
		texts := extractFile(file.path)
		if len(texts) == 0 {
			continue
		}
		item := Excerpt{Source: file.source, File: file.path}
		for _, text := range texts {
			if total+len(text) > maxTotalBytes {
				break
			}
			item.Texts = append(item.Texts, text)
			total += len(text)
			if len(item.Texts) >= maxPerFile {
				break
			}
		}
		if len(item.Texts) > 0 {
			out = append(out, item)
		}
		if total >= maxTotalBytes {
			break
		}
	}
	return out
}

func Format(excerpts []Excerpt) string {
	if len(excerpts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range excerpts {
		for _, text := range item.Texts {
			b.WriteString(string(item.Source))
			b.WriteString(": ")
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func extractFile(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var texts []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		texts = append(texts, extractUserTexts([]byte(line))...)
		if len(texts) >= maxPerFile {
			break
		}
	}
	return texts
}

func extractUserTexts(raw []byte) []string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	var out []string
	walkUser(value, "", &out)
	return out
}

func walkUser(value any, role string, out *[]string) {
	switch node := value.(type) {
	case map[string]any:
		next := role
		if s, ok := node["role"].(string); ok && s != "" {
			next = s
		}
		if t, ok := node["type"].(string); ok && t == "user" {
			next = "user"
		}
		if next == "user" {
			if s, ok := node["text"].(string); ok {
				appendUserText(s, out)
			}
			if s, ok := node["content"].(string); ok {
				appendUserText(s, out)
			}
		}
		for _, child := range node {
			walkUser(child, next, out)
		}
	case []any:
		for _, child := range node {
			walkUser(child, role, out)
		}
	}
}

func appendUserText(text string, out *[]string) {
	text = strings.TrimSpace(text)
	if text == "" || strings.Contains(text, envContextMark) {
		return
	}
	if utf8.RuneCountInString(text) > maxTextRunes {
		runes := []rune(text)
		text = string(runes[:maxTextRunes]) + "…"
	}
	*out = append(*out, text)
}
