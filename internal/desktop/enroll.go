package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/fsx"
)

// Enrollment is what a first run registered and what it left for the owner.
type Enrollment struct {
	Added   []string
	Pending []string // found, but missing a requirement such as Node.js
	Failed  []string // registration was attempted and refused
}

// enrollMarker records that init owns enrollment for this installation, so a
// rerun can finish a batch that stopped partway without touching agents the
// owner registered.
const enrollMarker = "init-enroll.json"

type enrollCandidate struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Installed  bool     `json:"installed"`
	Requires   []string `json:"requires"`
	Registered bool     `json:"registered"`
}

// EnrollRunning registers this computer's signed-in coding agents through the
// running installation, exactly as the first-run guide does: the worker
// installs each tool's adapter, the shared declaration gains the agent and the
// live service starts routing to it. An installation that already has agents
// belongs to its owner and is left alone; Resources manages later additions.
func EnrollRunning(ctx context.Context, installed *Installation, address string) (Enrollment, error) {
	return enrollRunning(ctx, installed, address, 500*time.Millisecond)
}

func enrollRunning(ctx context.Context, installed *Installation, address string, wait time.Duration) (Enrollment, error) {
	if err := localURL(address, false); err != nil {
		return Enrollment{}, err
	}
	client := &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	call := func(method, path string, input, output any) error {
		var body io.Reader
		if input != nil {
			raw, err := json.Marshal(input)
			if err != nil {
				return err
			}
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, address+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+installed.Token)
		if input != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(detail)))
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output)
	}
	// The identity probe answers before the application is serving. Wait for a
	// read that goes through it; never retry a registration blindly.
	for {
		var state struct{}
		err := call(http.MethodGet, "/state", nil, &state)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return Enrollment{}, fmt.Errorf("the service did not become ready: %w", err)
		case <-time.After(wait):
		}
	}
	marker := filepath.Join(installed.Paths.Root, enrollMarker)
	var progress struct {
		Done bool `json:"done"`
	}
	resuming := false
	if raw, err := os.ReadFile(marker); err == nil && json.Unmarshal(raw, &progress) == nil {
		if progress.Done {
			return Enrollment{}, nil
		}
		resuming = true
	}
	var status struct {
		AgentCount int `json:"agent_count"`
	}
	if err := call(http.MethodGet, "/console/desktop", nil, &status); err != nil {
		return Enrollment{}, err
	}
	if status.AgentCount > 0 && !resuming {
		return Enrollment{}, nil
	}
	var discovery struct {
		Agents []enrollCandidate `json:"agents"`
	}
	if err := call(http.MethodGet, "/console/desktop/agents", nil, &discovery); err != nil {
		return Enrollment{}, err
	}
	if installed.Paths.Root != "" {
		if err := fsx.WriteFile(marker, []byte("{\"done\":false}\n")); err != nil {
			return Enrollment{}, err
		}
	}
	type choice struct {
		CandidateID string `json:"candidate_id"`
		Default     bool   `json:"default,omitempty"`
	}
	var result Enrollment
	for _, candidate := range discovery.Agents {
		switch {
		case !candidate.Installed || candidate.Registered:
		case len(candidate.Requires) > 0:
			result.Pending = append(result.Pending, fmt.Sprintf("%s (needs %s)", candidate.Name, strings.Join(candidate.Requires, ", ")))
		default:
			// One tool per request: a failed adapter install leaves the
			// others registered and reported rather than unattempted.
			want := choice{CandidateID: candidate.ID, Default: status.AgentCount == 0 && len(result.Added) == 0}
			var after struct {
				AgentCount int `json:"agent_count"`
			}
			if err := call(http.MethodPost, "/console/desktop/agents", map[string]any{"agents": []choice{want}}, &after); err != nil {
				result.Failed = append(result.Failed, fmt.Sprintf("%s (%v)", candidate.Name, err))
				continue
			}
			if after.AgentCount <= status.AgentCount {
				result.Failed = append(result.Failed, candidate.Name+" (not confirmed)")
				continue
			}
			status.AgentCount = after.AgentCount
			result.Added = append(result.Added, candidate.Name)
		}
	}
	if len(result.Failed) == 0 && installed.Paths.Root != "" {
		if err := fsx.WriteFile(marker, []byte("{\"done\":true}\n")); err != nil {
			return result, err
		}
	}
	return result, nil
}
