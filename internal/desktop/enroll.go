package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Enrollment is what a first run registered and what it left for the owner.
type Enrollment struct {
	Added   []string
	Pending []string // found, but missing a requirement such as Node.js
}

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
	var status struct {
		AgentCount int `json:"agent_count"`
	}
	if err := call(http.MethodGet, "/console/desktop", nil, &status); err != nil {
		return Enrollment{}, err
	}
	if status.AgentCount > 0 {
		return Enrollment{}, nil
	}
	var discovery struct {
		Agents []enrollCandidate `json:"agents"`
	}
	if err := call(http.MethodGet, "/console/desktop/agents", nil, &discovery); err != nil {
		return Enrollment{}, err
	}
	type choice struct {
		CandidateID string `json:"candidate_id"`
		Default     bool   `json:"default,omitempty"`
	}
	var result Enrollment
	var chosen []choice
	for _, candidate := range discovery.Agents {
		switch {
		case !candidate.Installed || candidate.Registered:
		case len(candidate.Requires) > 0:
			result.Pending = append(result.Pending, fmt.Sprintf("%s (needs %s)", candidate.Name, strings.Join(candidate.Requires, ", ")))
		default:
			chosen = append(chosen, choice{CandidateID: candidate.ID, Default: len(chosen) == 0})
			result.Added = append(result.Added, candidate.Name)
		}
	}
	if len(chosen) == 0 {
		return result, nil
	}
	var after struct {
		AgentCount int `json:"agent_count"`
	}
	if err := call(http.MethodPost, "/console/desktop/agents", map[string]any{"agents": chosen}, &after); err != nil {
		return Enrollment{}, err
	}
	if after.AgentCount == 0 {
		return Enrollment{}, errors.New("the service did not confirm the new agents")
	}
	return result, nil
}
