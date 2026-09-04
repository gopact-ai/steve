package skills

import (
	"bufio"
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
	var d Description
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	inFront, first := false, true
	var para []string
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
				inFront = false
				continue
			}
			if k, v, ok := strings.Cut(line, ":"); ok {
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "name":
					d.Title = v
				case "description":
					d.Description = v
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
