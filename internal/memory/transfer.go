package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExportProjectFiles includes only this project's durable memory and receipt
// journals; the owner's global memory and other projects' audit rows stay home.
func ExportProjectFiles(dir, project string) (map[string][]byte, error) {
	if project == "" || strings.ContainsAny(project, "/\\") {
		return nil, fmt.Errorf("invalid project memory id")
	}
	out := map[string][]byte{}
	for _, suffix := range []string{".md", ".md.requests.jsonl", ".md.pending.json"} {
		name := project + suffix
		data, err := os.ReadFile(filepath.Join(dir, "projects", name))
		if err == nil {
			out[name] = data
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	var selected bytes.Buffer
	// Inherited history and this hub's new audit records are both durable
	// project history. Keep each original line intact across further moves.
	for _, path := range []string{filepath.Join(dir, "projects", project+".audit.jsonl"), filepath.Join(dir, "audit.jsonl")} {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			var entry auditLine
			if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
				return nil, err
			}
			if entry.Scope.Kind == KindProject && entry.Scope.Project == project {
				selected.Write(scanner.Bytes())
				selected.WriteByte('\n')
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	if selected.Len() > 0 {
		out[project+".audit.jsonl"] = selected.Bytes()
	}
	return out, nil
}
