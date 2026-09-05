package skills

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Description is what a skill says about itself in its SKILL.md: the
// name and description from its front matter when it has one, else the
// first heading and the first paragraph.
type Description struct {
	Title       string
	Description string
}

// Describe reads a skill directory's SKILL.md for its description. A
// skill that cannot be read describes itself as nothing.
func Describe(dir string) Description {
	f, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Description{}
	}
	defer f.Close()
	return DescribeText(f)
}

// DescribeText reads a SKILL.md's description from its text.
func DescribeText(r io.Reader) Description {
	var d Description
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	inFront, first := false, true
	var para []string
	// A front-matter value may be a block scalar ("description: >" or
	// "|-"), continuing on the indented lines below; block collects them.
	block, style := "", ""
	var blockLines []string
	flush := func() {
		if block == "" {
			return
		}
		text := strings.Join(blockLines, " ")
		if strings.HasPrefix(style, "|") {
			text = strings.Join(blockLines, "\n")
		}
		switch block {
		case "name":
			d.Title = strings.TrimSpace(text)
		case "description":
			d.Description = strings.TrimSpace(text)
		}
		block, style, blockLines = "", "", nil
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if first {
			first = false
			if line == "---" {
				inFront = true
				continue
			}
		}
		if inFront {
			if line == "---" {
				flush()
				inFront = false
				continue
			}
			if block != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || line == "") {
				if line != "" {
					blockLines = append(blockLines, strings.TrimSpace(line))
				}
				continue
			}
			flush()
			if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, " ") {
				key := strings.ToLower(strings.TrimSpace(k))
				v = strings.TrimSpace(v)
				if key == "name" || key == "description" {
					if v == ">" || v == ">-" || v == "|" || v == "|-" || v == ">+" || v == "|+" {
						block, style = key, v
						continue
					}
					v = strings.Trim(v, `"'`)
					if key == "name" {
						d.Title = v
					} else {
						d.Description = v
					}
				}
			}
			continue
		}
		if d.Description != "" {
			break
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			if d.Title == "" {
				d.Title = strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			}
			continue
		}
		if trimmed == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, trimmed)
	}
	if d.Description == "" && len(para) > 0 {
		d.Description = strings.Join(para, " ")
	}
	if r := []rune(d.Description); len(r) > 200 {
		d.Description = string(r[:200]) + "…"
	}
	return d
}

// Content is a skill's SKILL.md as written.
func Content(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
